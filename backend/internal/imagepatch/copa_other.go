//go:build !unix

package imagepatch

import (
	"context"

	copatypes "github.com/project-copacetic/copacetic/pkg/types"
)

// copaSupported is false because copa rebuilds Linux image layers through
// BuildKit, which a Windows container engine does not provide, and copa's
// pkg/tui does not compile for GOOS=windows.
const copaSupported = false

func copaPatchInternal(context.Context, *copatypes.Options) error {
	return ErrPatchUnsupportedPlatform
}

func resolvePatchedRef(string, string, string) (string, error) {
	return "", ErrPatchUnsupportedPlatform
}
