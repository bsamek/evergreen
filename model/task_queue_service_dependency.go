package model

// TODO Cross-variant dependencies
// TODO Dependencies other than success.

import (
	"fmt"
	"sync"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/model/host"
	"github.com/evergreen-ci/evergreen/model/task"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
)

type taskDistroDAGDispatchService struct {
	mu          sync.RWMutex
	distroID    string
	graph       *simple.DirectedGraph
	sorted      []graph.Node
	itemNodeMap map[string]graph.Node
	nodeItemMap map[int64]*TaskQueueItem
	taskGroups  map[string]taskGroupTasks
	ttl         time.Duration
	lastUpdated time.Time
}

type taskGroupTasks struct {
	id           string
	runningHosts int // number of hosts task group is currently running on
	maxHosts     int // number of hosts task group can run on
	tasks        []TaskQueueItem
}

// taskDistroDAGDispatchService creates a taskDistroDAGDispatchService from a slice of TaskQueueItems.
func newDistroTaskDAGDispatchService(distroID string, items []TaskQueueItem, ttl time.Duration) *taskDistroDAGDispatchService {
	t := &taskDistroDAGDispatchService{
		distroID: distroID,
		ttl:      ttl,
	}
	t.graph = simple.NewDirectedGraph()
	t.itemNodeMap = map[string]graph.Node{}
	t.nodeItemMap = map[int64]*TaskQueueItem{}
	t.taskGroups = map[string]taskGroupTasks{}
	if len(items) != 0 {
		t.rebuild(items)
	}

	return t
}

func (t *taskDistroDAGDispatchService) Refresh() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !shouldRefreshCached(t.ttl, t.lastUpdated) {
		return nil
	}

	taskQueue, err := FindDistroTaskQueue(t.distroID)
	if err != nil {
		return errors.WithStack(err)
	}

	taskQueueItems := taskQueue.Queue
	t.rebuild(taskQueueItems)

	return nil
}

func (t *taskDistroDAGDispatchService) addItem(item TaskQueueItem) {
	fmt.Printf("> adding item %s\n", item.Id)
	node := t.graph.NewNode()
	t.graph.AddNode(node)
	t.nodeItemMap[node.ID()] = &item
	t.itemNodeMap[item.Id] = node
}

func (t *taskDistroDAGDispatchService) getItemByNode(id int64) *TaskQueueItem {
	if item, ok := t.nodeItemMap[id]; ok {
		return item
	}
	grip.Error(message.Fields{
		"message": "programmer error, couldn't find node in map",
	})
	return nil
}

func (t *taskDistroDAGDispatchService) getNodeByItem(id string) graph.Node {
	if node, ok := t.itemNodeMap[id]; ok {
		return node
	}
	grip.Error(message.Fields{
		"message": "programmer error, couldn't find node in map",
	})
	return nil
}

func (t *taskDistroDAGDispatchService) addEdge(from string, to string) {
	fmt.Printf("> adding edge %s -> %s\n", from, to)
	edge := simple.Edge{
		F: simple.Node(t.itemNodeMap[from].ID()),
		T: simple.Node(t.itemNodeMap[to].ID()),
	}
	t.graph.SetEdge(edge)
}

func (t *taskDistroDAGDispatchService) rebuild(items []TaskQueueItem) {
	// TODO Add timing info
	// TODO Error handling?
	t.lastUpdated = time.Now()

	// Add items to the graph
	for _, item := range items {
		t.addItem(item)
	}

	// Persist task groups
	t.taskGroups = map[string]taskGroupTasks{}
	for _, item := range items {
		if item.Group != "" {
			id := compositeGroupId(item.Group, item.BuildVariant, item.Version)
			if _, ok := t.taskGroups[id]; !ok {
				t.taskGroups[id] = taskGroupTasks{
					id:       id,
					maxHosts: item.GroupMaxHosts,
					tasks:    []TaskQueueItem{item},
				}
			} else {
				taskGroup := t.taskGroups[id]
				taskGroup.tasks = append(taskGroup.tasks, item)
				t.taskGroups[id] = taskGroup
			}
		}
	}

	// Add edges for task groups of 1
	var currentItem TaskQueueItem
	var currentItemExists bool
	for _, taskGroup := range t.taskGroups {
		currentItemExists = false
		for _, item := range taskGroup.tasks {
			if item.GroupMaxHosts != 1 {
				break
			}
			if currentItemExists {
				t.addEdge(currentItem.Id, item.Id)
			}
			currentItem = item
			currentItemExists = true
		}
	}

	// Add edges for task dependencies
	for _, item := range items {
		for _, dep := range item.Dependencies {
			t.addEdge(item.Id, dep)
		}
	}

	// Sort the graph
	sorted, err := topo.Sort(t.graph)
	if err != nil {
		grip.Alert(message.WrapError(err, message.Fields{
			"message": "problem sorting tasks",
		}))
		return
	}
	t.sorted = sorted

	return
}

// FindNextTask returns the next dispatchable task in the queue.
func (t *taskDistroDAGDispatchService) FindNextTask(spec TaskSpec) *TaskQueueItem {
	t.mu.Lock()
	defer t.mu.Unlock()

	// If the host just ran a task group, give it one back
	if spec.Group != "" {
		taskGroup, ok := t.taskGroups[compositeGroupId(spec.Group, spec.BuildVariant, spec.Version)]
		if ok {
			if next := t.nextTaskGroupTask(taskGroup); next != nil {
				return next
			}
		}
		// If the task group is not present in the task group map, it has been dispatched,
		// so fall through to getting a task not in that group.
	}

	// Iterate through the topologically-sorted graph.
	for i := len(t.sorted) - 1; i >= 0; i-- {
		node := t.sorted[i]
		item := t.getItemByNode(node.ID())

		// If we get to a task that has unfulfilled dependencies, there are no more tasks
		// that are dispatchable in the in-memory queue.
		if len(t.graph.From(node)) > 0 {
			break
		}

		// If maxHosts is not set, this is not a task group.
		if item.GroupMaxHosts == 0 {
			return item
		}

		// For a task group task, do some arithmetic to see if it's dispatchable
		taskGroupID := compositeGroupId(item.Group, item.BuildVariant, item.Version)
		taskGroup := t.taskGroups[taskGroupID]
		if taskGroup.runningHosts < taskGroup.maxHosts {
			numHosts, err := host.NumHostsByTaskSpec(spec.Group, spec.BuildVariant, spec.ProjectID, spec.Version)
			if err != nil {
				grip.Error(message.WrapError(err, message.Fields{
					"message": "problem running NumHostsByTaskSpec query",
					"group":   spec.Group,
					"variant": spec.BuildVariant,
					"project": spec.ProjectID,
					"version": spec.Version,
				}))
				return nil
			}
			taskGroup.runningHosts = numHosts
			t.taskGroups[taskGroupID] = taskGroup
			if taskGroup.runningHosts < taskGroup.maxHosts {
				if next := t.nextTaskGroupTask(taskGroup); next != nil {
					return next
				}
			}
		}
	}

	// TODO Consider checking if the state of any tasks have changed, which could unblock
	// later tasks in the queue. Currently we just wait for the scheduler to rerun.

	return nil
}

func (t *taskDistroDAGDispatchService) nextTaskGroupTask(taskGroup taskGroupTasks) *TaskQueueItem {
	for i, nextTask := range taskGroup.tasks {
		if nextTask.IsDispatched == true {
			continue
		}

		nextTaskFromDB, err := task.FindOneId(nextTask.Id)
		if err != nil {
			grip.Error(message.WrapError(err, message.Fields{
				"message": "problem finding task in db",
				"task":    nextTask.Id,
			}))
			return nil
		}
		if nextTaskFromDB == nil {
			grip.Error(message.Fields{
				"message": "task from db not found",
				"task":    nextTask.Id,
			})
			return nil
		}

		if t.isBlockedSingleHostTaskGroup(taskGroup, nextTaskFromDB) {
			delete(t.taskGroups, taskGroup.id)
			return nil
		}

		// Cache dispatched status.
		t.taskGroups[taskGroup.id].tasks[i].IsDispatched = true
		taskGroup.tasks[i].IsDispatched = true

		if nextTaskFromDB.StartTime != util.ZeroTime {
			continue
		}
		// Don't cache dispatched status when returning the next TaskQueueItem - in case the task fails to start.
		return &nextTask
	}
	// If all the tasks have been dispatched, remove the taskGroup.
	delete(t.taskGroups, taskGroup.id)
	return nil
}

// isBlockedSingleHostTaskGroup checks if the task is running in a 1-host task group, has finished,
// and did not succeed. But rely on EndTask to block later tasks.
func (t *taskDistroDAGDispatchService) isBlockedSingleHostTaskGroup(taskGroup taskGroupTasks, dbTask *task.Task) bool {
	return taskGroup.maxHosts == 1 && !util.IsZeroTime(dbTask.FinishTime) && dbTask.Status != evergreen.TaskSucceeded
}
