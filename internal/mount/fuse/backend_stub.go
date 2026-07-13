//go:build !linux && !darwin

package fusemount

import (
	"context"
	"errors"
	"github.com/gopact-ai/9a/internal/mount"
)

type Backend struct{}

func New() *Backend { return &Backend{} }
func (*Backend) Available(context.Context) error {
	return errors.New("fuse is unsupported on this platform")
}
func (*Backend) Attach(context.Context, string, string, mount.Snapshot) (mount.Attachment, error) {
	return mount.Attachment{}, errors.New("fuse is unsupported")
}
func (*Backend) Update(context.Context, mount.Attachment, mount.Snapshot) (mount.Attachment, error) {
	return mount.Attachment{}, errors.New("fuse is unsupported")
}
func (*Backend) Inspect(context.Context, mount.Attachment, mount.Snapshot) (mount.Inspection, error) {
	return mount.Inspection{}, errors.New("fuse is unsupported")
}
func (*Backend) Detach(context.Context, mount.Attachment) error { return nil }
func (*Backend) Close(context.Context) error                    { return nil }
