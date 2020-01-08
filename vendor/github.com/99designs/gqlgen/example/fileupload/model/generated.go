// Code generated by github.com/99designs/gqlgen, DO NOT EDIT.

package model

import (
	"github.com/99designs/gqlgen/graphql"
)

// The `File` type, represents the response of uploading a file.
type File struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// The `UploadFile` type, represents the request for uploading a file with certain payload.
type UploadFile struct {
	ID   int            `json:"id"`
	File graphql.Upload `json:"file"`
}
