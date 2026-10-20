package agent

import (
	"strings"
	"testing"
)

func TestWithoutSecrets(t *testing.T) {
	got := withoutSecrets([]string{"PATH=/bin", "GITHUB_TOKEN=x", "GH_TOKEN=y", "CLAUDE_CODE_OAUTH_TOKEN=keep", "HOME=/h"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "GITHUB_TOKEN") || strings.Contains(joined, "GH_TOKEN=") {
		t.Errorf("GitHub tokens should be dropped: %v", got)
	}
	for _, want := range []string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=keep", "HOME=/h"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s should be kept: %v", want, got)
		}
	}
}
