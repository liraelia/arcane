//go:build unix

package imagepatch

import (
	"context"

	"emperror.dev/errors"
	"github.com/distribution/reference"
	"github.com/moby/buildkit/util/progress/progressui"
	copacommon "github.com/project-copacetic/copacetic/pkg/common"
	copapatch "github.com/project-copacetic/copacetic/pkg/patch"
	copatypes "github.com/project-copacetic/copacetic/pkg/types"
)

const copaSupported = true

func copaPatchInternal(ctx context.Context, opts *copatypes.Options) error {
	opts.Progress = progressui.QuietMode
	return copapatch.Patch(ctx, opts)
}

// resolvePatchedRef computes the patched reference with the exact same
// resolution copa applies internally, so the recorded ref matches the result.
func resolvePatchedRef(imageRef, patchedTag, suffix string) (string, error) {
	named, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return "", errors.WrapIf(err, "failed to parse image reference")
	}
	name, tag, err := copacommon.ResolvePatchedImageName(named, patchedTag, suffix)
	if err != nil {
		return "", errors.WrapIf(err, "failed to resolve patched image name")
	}
	return name + ":" + tag, nil
}
