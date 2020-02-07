package cloud

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/user"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2aws "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model/distro"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/mongodb/anser/bsonutil"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
)

const (
	EC2ErrorNotFound    = "InvalidInstanceID.NotFound"
	EC2DuplicateKeyPair = "InvalidKeyPair.Duplicate"
)

type MountPoint struct {
	VirtualName string `mapstructure:"virtual_name" json:"virtual_name,omitempty" bson:"virtual_name,omitempty"`
	DeviceName  string `mapstructure:"device_name" json:"device_name,omitempty" bson:"device_name,omitempty"`
	Size        int64  `mapstructure:"size" json:"size,omitempty" bson:"size,omitempty"`
	Iops        int64  `mapstructure:"iops" json:"iops,omitempty" bson:"iops,omitempty"`
	SnapshotID  string `mapstructure:"snapshot_id" json:"snapshot_id,omitempty" bson:"snapshot_id,omitempty"`
	VolumeType  string `mapstructure:"volume_type" json:"volume_type,omitempty" bson:"volume_type,omitempty"`
}

var (
	// bson fields for the EC2ProviderSettings struct
	AMIKey            = bsonutil.MustHaveTag(EC2ProviderSettings{}, "AMI")
	InstanceTypeKey   = bsonutil.MustHaveTag(EC2ProviderSettings{}, "InstanceType")
	SecurityGroupsKey = bsonutil.MustHaveTag(EC2ProviderSettings{}, "SecurityGroupIDs")
	KeyNameKey        = bsonutil.MustHaveTag(EC2ProviderSettings{}, "KeyName")
	MountPointsKey    = bsonutil.MustHaveTag(EC2ProviderSettings{}, "MountPoints")
)

var (
	// bson fields for the EC2SpotSettings struct
	BidPriceKey = bsonutil.MustHaveTag(EC2ProviderSettings{}, "BidPrice")
)

var (
	// bson fields for the MountPoint struct
	VirtualNameKey = bsonutil.MustHaveTag(MountPoint{}, "VirtualName")
	DeviceNameKey  = bsonutil.MustHaveTag(MountPoint{}, "DeviceName")
	SizeKey        = bsonutil.MustHaveTag(MountPoint{}, "Size")
	VolumeTypeKey  = bsonutil.MustHaveTag(MountPoint{}, "VolumeType")
)

// type/consts for price evaluation based on OS
type osType string

const (
	osLinux   osType = "Linux/UNIX"
	osSUSE    osType = "SUSE Linux"
	osWindows osType = "Windows"
)

// regionFullname takes the API ID of amazon region and returns the
// full region name. For instance, "us-west-1" becomes "US West (N. California)".
// This is necessary as the On Demand pricing endpoint uses the full name, unlike
// the rest of the API. THIS FUNCTION ONLY HANDLES U.S. REGIONS.
func regionFullname(region string) (string, error) {
	switch region {
	case "us-east-1":
		return "US East (N. Virginia)", nil
	case "us-west-1":
		return "US West (N. California)", nil
	case "us-west-2":
		return "US West (Oregon)", nil
	}
	return "", errors.Errorf("region %v not supported for On Demand cost calculation", region)
}

// AztoRegion takes an availability zone and returns the region id.
func AztoRegion(az string) string {
	// an amazon region is just the availability zone minus the final letter
	return az[:len(az)-1]
}

// returns the format of os name expected by EC2 On Demand billing data,
// bucking the normal AWS API naming scheme.
func osBillingName(os osType) string {
	if os == osLinux {
		return "Linux"
	}
	return string(os)
}

//ec2StatusToEvergreenStatus returns a "universal" status code based on EC2's
//provider-specific status codes.
func ec2StatusToEvergreenStatus(ec2Status string) CloudStatus {
	switch ec2Status {
	case ec2.InstanceStateNamePending:
		return StatusInitializing
	case ec2.InstanceStateNameRunning:
		return StatusRunning
	case ec2.InstanceStateNameStopped:
		return StatusStopped
	case ec2.InstanceStateNameTerminated, ec2.InstanceStateNameShuttingDown:
		return StatusTerminated
	default:
		grip.Error(message.Fields{
			"message": "got an unknown ec2 state name",
			"status":  ec2Status,
		})
		return StatusUnknown
	}
}

// expireInDays creates an expire-on string in the format YYYY-MM-DD for numDays days
// in the future.
func expireInDays(numDays int) string {
	return time.Now().AddDate(0, 0, numDays).Format("2006-01-02")
}

// makeTags populates a slice of tags based on a host object, which contain keys
// for the user, owner, hostname, and if it's a spawnhost or not.
func makeTags(intentHost *host.Host) []host.Tag {
	// get requester host name
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	// get requester user name
	var username string
	user, err := user.Current()
	if err != nil {
		username = "unknown"
	} else {
		username = user.Name
	}

	// The expire-on tag is required by MongoDB's AWS reaping policy.
	// The reaper is an external script that scans every ec2 instance for an expire-on tag,
	// and if that tag is passed the reaper terminates the host. This reaping occurs to
	// ensure that any hosts that we forget about or that fail to terminate do not stay alive
	// forever.
	expireOn := expireInDays(evergreen.HostExpireDays)
	if intentHost.UserHost {
		// If this is a spawn host, use a different expiration date.
		expireOn = expireInDays(evergreen.SpawnHostExpireDays)
	}

	systemTags := []host.Tag{
		host.Tag{Key: "name", Value: intentHost.Id, CanBeModified: false},
		host.Tag{Key: "distro", Value: intentHost.Distro.Id, CanBeModified: false},
		host.Tag{Key: "evergreen-service", Value: hostname, CanBeModified: false},
		host.Tag{Key: "username", Value: username, CanBeModified: false},
		host.Tag{Key: "owner", Value: intentHost.StartedBy, CanBeModified: false},
		host.Tag{Key: "mode", Value: "production", CanBeModified: false},
		host.Tag{Key: "start-time", Value: intentHost.CreationTime.Format(evergreen.NameTimeFormat), CanBeModified: false},
		host.Tag{Key: "expire-on", Value: expireOn, CanBeModified: false},
	}

	if intentHost.UserHost {
		systemTags = append(systemTags, host.Tag{Key: "mode", Value: "testing", CanBeModified: false})
	}

	if isHostSpot(intentHost) {
		systemTags = append(systemTags, host.Tag{Key: "spot", Value: "true", CanBeModified: false})
	}

	// Add Evergreen-generated tags to host object
	intentHost.AddTags(systemTags)

	return intentHost.InstanceTags
}

func timeTilNextEC2Payment(h *host.Host) time.Duration {
	if usesHourlyBilling(h) {
		return timeTilNextHourlyPayment(h)
	}

	upTime := time.Since(h.StartTime)
	if upTime < time.Minute {
		return time.Minute - upTime
	}

	return time.Second
}

func usesHourlyBilling(h *host.Host) bool { return !strings.Contains(h.Distro.Arch, "linux") }

// Determines how long until a payment is due for the specified host, for hosts
// that bill hourly. Returns the next time that it would take for the host to be
// up for an integer number of hours
func timeTilNextHourlyPayment(host *host.Host) time.Duration {
	now := time.Now()
	var startTime time.Time
	if host.StartTime.After(host.CreationTime) {
		startTime = host.StartTime
	} else {
		startTime = host.CreationTime
	}

	// the time since the host was started
	timeSinceCreation := now.Sub(startTime)

	// the hours since the host was created, rounded up
	hoursRoundedUp := time.Duration(math.Ceil(timeSinceCreation.Hours()))

	// the next round number of hours the host will have been up - the time
	// that the next payment will be due
	nextPaymentTime := startTime.Add(hoursRoundedUp * time.Hour)

	return nextPaymentTime.Sub(now)
}

func expandUserData(userData string, expansions map[string]string) (string, error) {
	exp := util.NewExpansions(expansions)
	expanded, err := exp.ExpandString(userData)
	if err != nil {
		return "", errors.Wrap(err, "error expanding userdata script")
	}
	return expanded, nil
}

func cacheHostData(ctx context.Context, h *host.Host, instance *ec2.Instance, client AWSClient) error {
	if instance.Placement == nil || instance.Placement.AvailabilityZone == nil {
		return errors.New("instance missing availability zone")
	}
	if instance.LaunchTime == nil {
		return errors.New("instance missing launch time")
	}
	if instance.PublicDnsName == nil {
		return errors.New("instance missing public dns name")
	}
	h.Zone = *instance.Placement.AvailabilityZone
	h.StartTime = *instance.LaunchTime
	h.Host = *instance.PublicDnsName
	h.Volumes = makeVolumeAttachments(instance.BlockDeviceMappings)

	var err error
	if h.ComputeCostPerHour == 0 {
		ec2Settings := &EC2ProviderSettings{}
		err := ec2Settings.FromDistroSettings(h.Distro, "")
		if err != nil {
			return errors.Wrapf(err, "error getting EC2 settings for host '%s'", h.Id)
		}
		h.ComputeCostPerHour, _, err = pkgCachingPriceFetcher.getLatestSpotCostForInstance(ctx, client, ec2Settings, getOsName(h), h.Zone)
		if err != nil {
			return errors.Wrapf(err, "can't get pricing for host '%s'", h.Id)
		}
	}

	h.VolumeTotalSize, err = getVolumeSize(ctx, client, h)
	if err != nil {
		return errors.Wrapf(err, "error getting volume size for host %s", h.Id)
	}

	if err = h.CacheHostData(); err != nil {
		return errors.Wrap(err, "error updating host document in db")
	}

	// set IPv6 address, if applicable
	for _, networkInterface := range instance.NetworkInterfaces {
		if len(networkInterface.Ipv6Addresses) > 0 {
			err = h.SetIPv6Address(*networkInterface.Ipv6Addresses[0].Ipv6Address)
			if err != nil {
				return errors.Wrap(err, "error setting ipv6 address")
			}
			break
		}
	}

	return nil
}

// ebsRegex extracts EBS Price JSON data from Amazon's UI.
var ebsRegex = regexp.MustCompile(`(?s)callback\((.*)\)`)

// odInfo is an internal type for keying hosts by the attributes that affect billing.
type odInfo struct {
	os       string
	instance string
	region   string
}

// Terms is an internal type for loading price API results into.
type Terms struct {
	OnDemand map[string]map[string]struct {
		PriceDimensions map[string]struct {
			PricePerUnit struct {
				USD string
			}
		}
	}
}

func getGeneratedDeviceNameForVolume(ctx context.Context, isWindowsHost bool) (string, error) {
	deviceName := ""
	err := util.Retry(
		ctx,
		func() (bool, error) {
			deviceName = generateDeviceNameForVolume(isWindowsHost)
			exists, err := host.HostExistsWithVolumeWithDeviceName(deviceName)
			if err != nil {
				return true, errors.Wrapf(err, "error checking if device name already exists")
			}
			if !exists {
				return false, nil
			}
			return true, errors.New("generated device name already exists")
		}, 500, 1, 10)

	return deviceName, err
}

// formats /dev/sd[f-p]and xvd[f-p] taken from https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/device_naming.html
func generateDeviceNameForVolume(isWindowsHost bool) string {
	letters := "fghijklmnop"
	rand.Seed(time.Now().Unix())
	pattern := "/dev/sd%c"
	if isWindowsHost {
		pattern = "xvd%c"
	}

	return fmt.Sprintf(pattern, letters[rand.Intn(len(letters))])
}

func makeBlockDeviceMappings(mounts []MountPoint) ([]*ec2aws.BlockDeviceMapping, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	mappings := []*ec2aws.BlockDeviceMapping{}
	for _, mount := range mounts {
		if mount.DeviceName == "" {
			return nil, errors.New("missing device name")
		}
		if mount.VirtualName == "" && mount.Size == 0 {
			return nil, errors.New("must provide either a virtual name or an EBS size")
		}

		m := &ec2aws.BlockDeviceMapping{
			DeviceName: aws.String(mount.DeviceName),
		}
		// Without a virtual name, this is EBS
		if mount.VirtualName == "" {
			m.Ebs = &ec2aws.EbsBlockDevice{
				DeleteOnTermination: aws.Bool(true),
				VolumeSize:          aws.Int64(mount.Size),
				VolumeType:          aws.String(ec2aws.VolumeTypeGp2),
			}
			if mount.Iops != 0 {
				m.Ebs.Iops = aws.Int64(mount.Iops)
			}
			if mount.SnapshotID != "" {
				m.Ebs.SnapshotId = aws.String(mount.SnapshotID)
			}
			if mount.VolumeType != "" {
				m.Ebs.VolumeType = aws.String(mount.VolumeType)
			}
		} else { // With a virtual name, this is an instance store
			m.VirtualName = aws.String(mount.VirtualName)
		}
		mappings = append(mappings, m)
	}
	return mappings, nil
}

func makeBlockDeviceMappingsTemplate(mounts []MountPoint) ([]*ec2aws.LaunchTemplateBlockDeviceMappingRequest, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	mappings := []*ec2aws.LaunchTemplateBlockDeviceMappingRequest{}
	for _, mount := range mounts {
		if mount.DeviceName == "" {
			return nil, errors.New("missing device name")
		}
		if mount.VirtualName == "" && mount.Size == 0 {
			return nil, errors.New("must provide either a virtual name or an EBS size")
		}

		m := &ec2aws.LaunchTemplateBlockDeviceMappingRequest{
			DeviceName: aws.String(mount.DeviceName),
		}
		// Without a virtual name, this is EBS
		if mount.VirtualName == "" {
			m.Ebs = &ec2aws.LaunchTemplateEbsBlockDeviceRequest{
				DeleteOnTermination: aws.Bool(true),
				VolumeSize:          aws.Int64(mount.Size),
				VolumeType:          aws.String(ec2aws.VolumeTypeGp2),
			}
			if mount.Iops != 0 {
				m.Ebs.Iops = aws.Int64(mount.Iops)
			}
			if mount.SnapshotID != "" {
				m.Ebs.SnapshotId = aws.String(mount.SnapshotID)
			}
			if mount.VolumeType != "" {
				m.Ebs.VolumeType = aws.String(mount.VolumeType)
			}
		} else { // With a virtual name, this is an instance store
			m.VirtualName = aws.String(mount.VirtualName)
		}
		mappings = append(mappings, m)
	}
	return mappings, nil
}

func makeVolumeAttachments(devices []*ec2.InstanceBlockDeviceMapping) []host.VolumeAttachment {
	attachments := []host.VolumeAttachment{}
	for _, device := range devices {
		if device.Ebs != nil && device.Ebs.VolumeId != nil && device.DeviceName != nil {
			attachments = append(attachments, host.VolumeAttachment{
				VolumeID:   *device.Ebs.VolumeId,
				DeviceName: *device.DeviceName,
			})
		}
	}
	return attachments
}

func validateEc2CreateTemplateResponse(createTemplateResponse *ec2aws.CreateLaunchTemplateOutput) error {
	if createTemplateResponse == nil || createTemplateResponse.LaunchTemplate == nil {
		return errors.New("create template response launch template is nil")
	}

	catcher := grip.NewBasicCatcher()
	if createTemplateResponse.LaunchTemplate.LaunchTemplateId == nil || len(*createTemplateResponse.LaunchTemplate.LaunchTemplateId) == 0 {
		catcher.Add(errors.New("create template response has no template identifier"))
	}

	if createTemplateResponse.LaunchTemplate.LatestVersionNumber == nil {
		catcher.Add(errors.New("create template response has no latest version"))
	}

	return catcher.Resolve()
}

func ec2CreateFleetResponseContainsInstance(createFleetResponse *ec2aws.CreateFleetOutput) bool {
	if createFleetResponse == nil {
		return false
	}

	if len(createFleetResponse.Instances) == 0 || len(createFleetResponse.Instances[0].InstanceIds) == 0 {
		return false
	}

	return true
}

func validateEc2DescribeInstancesOutput(describeInstancesResponse *ec2aws.DescribeInstancesOutput) error {
	catcher := grip.NewBasicCatcher()
	for _, reservation := range describeInstancesResponse.Reservations {
		if len(reservation.Instances) == 0 {
			catcher.Add(errors.New("reservation missing instance"))
		} else {
			instance := reservation.Instances[0]
			catcher.NewWhen(instance.InstanceId == nil, "instance missing instance id")
			catcher.NewWhen(instance.State == nil || instance.State.Name == nil || len(*instance.State.Name) == 0, "instance missing state name")
		}
	}

	return catcher.Resolve()
}

func validateEc2DescribeSubnetsOutput(describeSubnetsOutput *ec2aws.DescribeSubnetsOutput) error {
	if describeSubnetsOutput == nil {
		return errors.New("describe subnets response is nil")
	}

	if len(describeSubnetsOutput.Subnets) == 0 {
		return errors.New("describe subnets response contains no subnets")
	}

	for _, subnet := range describeSubnetsOutput.Subnets {
		if subnet.SubnetId == nil || *subnet.SubnetId == "" {
			return errors.New("describe subnets response contains a subnet without an ID")
		}
	}

	return nil
}

func validateEc2DescribeVpcsOutput(describeVpcsOutput *ec2aws.DescribeVpcsOutput) error {
	if describeVpcsOutput == nil {
		return errors.New("describe VPCs response is nil")
	}
	if len(describeVpcsOutput.Vpcs) == 0 {
		return errors.New("describe VPCs response contains no VPCs")
	}
	if describeVpcsOutput.Vpcs[0].VpcId == nil || *describeVpcsOutput.Vpcs[0].VpcId == "" {
		return errors.New("describe VPCs response contains a VPC with no VPC ID")
	}

	return nil
}

func IsEc2Provider(provider string) bool {
	return provider == evergreen.ProviderNameEc2Auto ||
		provider == evergreen.ProviderNameEc2OnDemand ||
		provider == evergreen.ProviderNameEc2Spot ||
		provider == evergreen.ProviderNameEc2Fleet
}

// Get EC2 region from a distro
func getEC2ManagerOptions(d distro.Distro) (ManagerOpts, error) {
	opts := ManagerOpts{}

	s := &EC2ProviderSettings{}
	if err := s.FromDistroSettings(d, ""); err != nil {
		return ManagerOpts{}, errors.Wrapf(err, "error getting EC2 provider settings from distro")
	}

	opts.Provider = d.Provider
	opts.Region = s.Region
	opts.ProviderKey = s.AWSKeyID
	opts.ProviderSecret = s.AWSSecret

	if opts.Region == "" {
		opts.Region = evergreen.DefaultEC2Region
	}

	return opts, nil
}

// Get EC2 key and secret from the AWS configuration for the given region
func GetEC2Key(region string, s *evergreen.Settings) (string, string, error) {
	// Get key and secret for specified region
	var key, secret string
	for _, k := range s.Providers.AWS.EC2Keys {
		if k.Region == region {
			key = k.Key
			secret = k.Secret

			// Error if key or secret are blank
			if key == "" || secret == "" {
				return "", "", errors.New("AWS ID and Secret must not be blank")
			}
			return key, secret, nil
		}
	}

	// Error if region is missing from config
	if key == "" || secret == "" {
		return "", "", errors.Errorf("Unable to find region '%s' in config", region)
	}

	return key, secret, nil
}

func validateEC2HostModifyOptions(h *host.Host, opts host.HostModifyOptions) error {
	if opts.InstanceType != "" && h.Status != evergreen.HostStopped {
		return errors.New("host must be stopped to modify instance typed")
	}
	if h.ExpirationTime.Add(opts.AddHours).Sub(time.Now()) > MaxSpawnHostExpirationDurationHours {
		return errors.Errorf("cannot extend host '%s' expiration by '%s' -- maximum host duration is limited to %s", h.Id, opts.AddHours.String(), MaxSpawnHostExpirationDurationHours.String())
	}

	return nil
}

// addSSHKey adds an SSH key for the given client. If an SSH key already exists
// with the given name, this no-ops.
func addSSHKey(ctx context.Context, client AWSClient, pair evergreen.SSHKeyPair) error {
	if _, err := client.ImportKeyPair(ctx, &ec2.ImportKeyPairInput{
		KeyName:           aws.String(pair.Name),
		PublicKeyMaterial: []byte(pair.Public),
	}); err != nil {
		if ec2err, ok := err.(awserr.Error); ok && ec2err.Code() == EC2DuplicateKeyPair {
			return nil
		}
		return errors.Wrap(err, "could not add new SSH key")
	}
	return nil
}
