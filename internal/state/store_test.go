package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStorePersistsReservationsAndCompletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executors.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reservation := Environment{OperationID: "op-1", InputDigest: "digest", Region: "us-west-2"}
	if _, err := store.Reserve(reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reserve(Environment{OperationID: "op-1", InputDigest: "other"}); !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("replay with different inputs = %v", err)
	}
	if _, err := store.Complete("op-1", "mvm-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunHookDelivered("mvm-1"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.ByHandle("mvm-1")
	if !ok || got.OperationID != "op-1" || !got.RunHookDelivered {
		t.Fatalf("reopened record = %+v, %v", got, ok)
	}
	if again, _ := reopened.Reserve(reservation); again.Handle != "mvm-1" {
		t.Fatalf("reserve of a completed operation = %+v", again)
	}
	if err := reopened.Forget("op-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.ByOperation("op-1"); ok {
		t.Fatal("forgotten record still present")
	}
}

func TestByHandleIgnoresUnfinishedReservations(t *testing.T) {
	store, _ := Open(filepath.Join(t.TempDir(), "executors.json"))
	_, _ = store.Reserve(Environment{OperationID: "op-1"})
	if _, ok := store.ByHandle(""); ok {
		t.Fatal("an empty handle matched a reservation")
	}
}

func TestOpenRefusesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executors.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt state was accepted")
	}
}
