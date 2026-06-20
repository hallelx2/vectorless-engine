package queue

import (
	"errors"
	"fmt"
	"testing"
)

func TestPermanentWrapsAndUnwraps(t *testing.T) {
	base := errors.New("encrypted PDF")
	perm := Permanent(base)

	if !IsPermanent(perm) {
		t.Fatal("IsPermanent should be true for a wrapped permanent error")
	}
	if !errors.Is(perm, base) {
		t.Fatal("errors.Is should still match the wrapped cause")
	}
	if perm.Error() != base.Error() {
		t.Fatalf("Error() = %q, want %q", perm.Error(), base.Error())
	}
}

func TestPermanentDetectedThroughWrapping(t *testing.T) {
	// A permanent error wrapped again with fmt.Errorf("%w") must still be
	// detectable — the ingest pipeline returns queue.Permanent(...) which a
	// caller may decorate with stage context.
	wrapped := fmt.Errorf("parse: %w", Permanent(errors.New("malformed")))
	if !IsPermanent(wrapped) {
		t.Fatal("IsPermanent should see through an outer fmt.Errorf wrap")
	}
}

func TestPermanentOfNilIsNil(t *testing.T) {
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must be nil so success paths stay clean")
	}
	if IsPermanent(nil) {
		t.Fatal("IsPermanent(nil) must be false")
	}
}

func TestOrdinaryErrorIsNotPermanent(t *testing.T) {
	if IsPermanent(errors.New("storage: object not found")) {
		t.Fatal("a plain (transient) error must not be classified permanent")
	}
}
