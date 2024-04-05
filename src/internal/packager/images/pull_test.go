package images

import (
	"testing"

	"github.com/defenseunicorns/zarf/src/pkg/transform"
	"github.com/stretchr/testify/require"
)

//TODO test manifest list, test when no tag

func TestGetImages(t *testing.T) {
	// Not sure this can be a real test without e2e or mocking. The risk for flaking is high
	refInfo, err := transform.ParseImageRef("ghcr.io/defenseunicorns/zarf/agent:v0.32.6@sha256:05a82656df5466ce17c3e364c16792ae21ce68438bfe06eeab309d0520c16b48")
	require.NoError(t, err)
	_, err = GetImagesFromIndexSha(refInfo)

}
