package bootstrap

import (
	"os"
	"regexp"
	"testing"
)

// The justfile stamps Version with -X <package>.Version. A package path that
// is not this one is silently ignored by the linker, and the binary reports
// "dev", so every -X in the justfile must name this package.
func TestJustfileStampsThisPackagesVersion(t *testing.T) {
	data, err := os.ReadFile("../../justfile")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile(`-X (\S+)\.Version=`).FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatal("justfile stamps no Version")
	}
	for _, m := range matches {
		if m[1] != "github.com/tacplus-dev/operator/pkg/bootstrap" {
			t.Errorf("justfile stamps %s.Version, want github.com/tacplus-dev/operator/pkg/bootstrap.Version", m[1])
		}
	}
}
