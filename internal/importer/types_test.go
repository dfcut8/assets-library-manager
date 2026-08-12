package importer

import (
	"crypto/sha256"
	"errors"
	"testing"
)

func TestIDRoundTrip(t *testing.T) {
	t.Parallel()

	id, err := ParseID("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("ParseID() error = %v", err)
	}
	if got := id.String(); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("ID.String() = %q", got)
	}
	if id.IsZero() {
		t.Fatal("parsed ID is zero")
	}
	generated, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	if generated.IsZero() {
		t.Fatal("NewID() returned zero")
	}
}

func TestParseIDRejectsNonCanonicalValues(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"", "0123456789abcdef", "0123456789ABCDEF0123456789ABCDEF",
		"g123456789abcdef0123456789abcdef",
	} {
		if _, err := ParseID(value); err == nil {
			t.Errorf("ParseID(%q) succeeded", value)
		}
	}
}

func TestDigestRoundTrip(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("asset"))
	digest := NewDigest(sum)
	parsed, err := ParseDigest(digest.String())
	if err != nil {
		t.Fatalf("ParseDigest() error = %v", err)
	}
	if parsed != digest {
		t.Fatalf("ParseDigest() = %v, want %v", parsed, digest)
	}
	bytes := parsed.Bytes()
	bytes[0] ^= 0xff
	if parsed != digest {
		t.Fatal("Digest.Bytes() returned aliased storage")
	}
}

func TestRelativePathValidation(t *testing.T) {
	t.Parallel()

	if _, err := NewSourcePath("image.PNG"); err != nil {
		t.Fatalf("NewSourcePath() error = %v", err)
	}
	if _, err := NewManagedPath("character/hero--0123456789ab.png"); err != nil {
		t.Fatalf("NewManagedPath() error = %v", err)
	}
	for _, value := range []string{"", ".", "../escape.png", "nested/image.png", `C:\image.png`, `link\image.png`} {
		if _, err := NewSourcePath(value); err == nil {
			t.Errorf("NewSourcePath(%q) succeeded", value)
		}
	}
	for _, value := range []string{"", ".", "../escape.png", "/root.png", `.staging/item.stage`, `C:/image.png`} {
		if _, err := NewManagedPath(value); err == nil {
			t.Errorf("NewManagedPath(%q) succeeded", value)
		}
	}
}

func TestStagedPathValidation(t *testing.T) {
	t.Parallel()

	id, err := ParseID("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	stagedPath, err := NewStagedPath(id)
	if err != nil {
		t.Fatalf("NewStagedPath() error = %v", err)
	}
	if got := stagedPath.String(); got != "0123456789abcdef0123456789abcdef.stage" {
		t.Fatalf("StagedPath.String() = %q", got)
	}
	if _, err := ParseStagedPath(stagedPath.String()); err != nil {
		t.Fatalf("ParseStagedPath() error = %v", err)
	}
	for _, value := range []string{"item.stage", "0123456789ABCDEF0123456789ABCDEF.stage", stagedPath.String() + ".bak"} {
		if _, err := ParseStagedPath(value); err == nil {
			t.Errorf("ParseStagedPath(%q) succeeded", value)
		}
	}
}

func TestItemStateTransitions(t *testing.T) {
	t.Parallel()

	valid := []ItemState{
		ItemStateDiscovered, ItemStateStaged, ItemStateAnalyzing, ItemStateCommitting,
		ItemStateReady, ItemStateDuplicate, ItemStateBlocked, ItemStateFailed,
	}
	for _, state := range valid {
		if !state.Valid() {
			t.Errorf("%q is not valid", state)
		}
	}
	if ItemStateUnknown.Valid() {
		t.Fatal("unknown item state is valid")
	}
	if !ItemStateDiscovered.CanTransitionTo(ItemStateStaged) ||
		!ItemStateCommitting.CanTransitionTo(ItemStateReady) ||
		!ItemStateFailed.CanTransitionTo(ItemStateDiscovered) {
		t.Fatal("expected workflow transition was rejected")
	}
	if ItemStateReady.CanTransitionTo(ItemStateDiscovered) ||
		ItemStateDuplicate.CanTransitionTo(ItemStateReady) {
		t.Fatal("terminal state accepted a transition")
	}
	if !ItemStateReady.Terminal() || !ItemStateDuplicate.Terminal() || ItemStateFailed.Terminal() {
		t.Fatal("terminal classification is incorrect")
	}
}

func TestStableWorkflowErrors(t *testing.T) {
	t.Parallel()

	wrapped := errors.New("outer: " + ErrSourceChanged.Error())
	if errors.Is(wrapped, ErrSourceChanged) {
		t.Fatal("errors.Is matched text instead of identity")
	}
}

func TestValidateErrorFieldsRejectsUnknownCodesAndOversizedMessages(t *testing.T) {
	t.Parallel()

	if err := ValidateErrorFields(ErrorCodeStorage, "bounded failure"); err != nil {
		t.Fatalf("ValidateErrorFields() error = %v", err)
	}
	if err := ValidateErrorFields(ErrorCode("invented"), "failure"); err == nil {
		t.Fatal("ValidateErrorFields() accepted unknown code")
	}
	message := make([]byte, maxErrorMessage+1)
	if err := ValidateErrorFields(ErrorCodeInternal, string(message)); err == nil {
		t.Fatal("ValidateErrorFields() accepted oversized message")
	}
}
