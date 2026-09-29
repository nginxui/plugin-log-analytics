package apierr

import (
	"errors"
	"testing"
)

func TestErrorFillsPlaceholders(t *testing.T) {
	base := NewScope("geolite").New(60000, "failed to download: {0}")
	err := WithParams(base, "status code: 500")

	if got, want := err.Error(), "failed to download: status code: 500"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}

	var coded *Error
	if !errors.As(err, &coded) || coded.Code != 60000 || coded.Scope != "geolite" {
		t.Fatalf("unexpected error %#v", err)
	}
	if len(base.(*Error).Params) != 0 {
		t.Fatal("WithParams must not modify the original error")
	}
}

func TestWithParamsKeepsPlainErrors(t *testing.T) {
	plain := errors.New("boom")
	if WithParams(plain, "x") != plain {
		t.Fatal("a plain error must come back unchanged")
	}
}
