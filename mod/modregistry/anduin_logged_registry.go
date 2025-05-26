package modregistry

import (
	"context"
	"io"
	"time"

	"cuelabs.dev/go/oci/ociregistry"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type loggedRegistry struct {
	ociregistry.Interface
}

func (reg *loggedRegistry) PushBlob(ctx context.Context, repo string, desc ocispec.Descriptor, r io.Reader) (ocispec.Descriptor, error) {
	start := time.Now()
	title := desc.Annotations[ocispec.AnnotationTitle]
	logf("Pushing blob for repo %s. Title: %s. Media Type: %s", repo, title, desc.MediaType)
	defer func() {
		logf("Finished pushing blob for repo %s. Title: %s. Media Type: %s. Elapsed %s", repo, title, desc.MediaType, time.Since(start))
	}()
	return reg.Interface.PushBlob(ctx, repo, desc, r)
}
