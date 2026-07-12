package mount

import (
	"context"
	"io/fs"
)

type File struct {
	Path string
	Mode fs.FileMode
	Data []byte
}

type Skill struct {
	Name, CapabilityID string
	Revision           int64
	Files              []File
}

type Backend interface {
	Publish(context.Context, string, Skill) error
	Remove(context.Context, string, Skill) error
}
