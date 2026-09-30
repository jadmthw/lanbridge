package selftest

import (
	"bytes"
	"context"
	"testing"
)

func TestSelftest(t *testing.T) {
	var out bytes.Buffer
	if err := Run(context.Background(), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
}
