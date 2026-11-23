package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestFlagValidationBeforeDatabaseConnection(t *testing.T) {
	t.Setenv("DBBRANCH_ADMIN_URL", "")
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"destroy", "--repo-id", "1", "--env", "preview"}, "positive repository and PR"},
		{[]string{"gc", "--repo-id", "1", "--env", "preview", "--older-than", "0s"}, "positive duration"},
		{[]string{"connection", "--repo-id", "1", "--env", "preview", "--pr", "3"}, "--id and --file"},
	}
	for _, tc := range tests {
		var out bytes.Buffer
		err := run(context.Background(), tc.args, &out, &out)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}
