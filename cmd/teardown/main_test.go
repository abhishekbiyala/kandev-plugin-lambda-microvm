package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
)

// fakeAPI serves canned pages and records what the tool asked it to do.
type fakeAPI struct {
	pages      []*lambdamicrovms.ListMicrovmsOutput
	listCalls  int
	terminated []string
	deleted    []string
	states     map[string]types.MicrovmState
	// getErr makes a read fail for one environment, which must not be read as
	// "terminated".
	getErr     map[string]error
	listErr    error
	termErr    map[string]error
	deleteErr  map[string]error
	getInvoked int
}

func (f *fakeAPI) ListMicrovms(_ context.Context, in *lambdamicrovms.ListMicrovmsInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.ListMicrovmsOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Honour the token so a tool that ignores it re-reads page one forever, which
	// is the failure this test exists to catch.
	page := 0
	if in.NextToken != nil {
		page = 1
	}
	f.listCalls++
	if page >= len(f.pages) {
		return &lambdamicrovms.ListMicrovmsOutput{}, nil
	}
	return f.pages[page], nil
}

func (f *fakeAPI) TerminateMicrovm(_ context.Context, in *lambdamicrovms.TerminateMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.TerminateMicrovmOutput, error) {
	id := aws.ToString(in.MicrovmIdentifier)
	f.terminated = append(f.terminated, id)
	if err := f.termErr[id]; err != nil {
		return nil, err
	}
	return &lambdamicrovms.TerminateMicrovmOutput{}, nil
}

func (f *fakeAPI) GetMicrovm(_ context.Context, in *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	f.getInvoked++
	id := aws.ToString(in.MicrovmIdentifier)
	if err := f.getErr[id]; err != nil {
		return nil, err
	}
	return &lambdamicrovms.GetMicrovmOutput{State: f.states[id]}, nil
}

func (f *fakeAPI) DeleteMicrovmImage(_ context.Context, in *lambdamicrovms.DeleteMicrovmImageInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.DeleteMicrovmImageOutput, error) {
	arn := aws.ToString(in.ImageIdentifier)
	f.deleted = append(f.deleted, arn)
	if err := f.deleteErr[arn]; err != nil {
		return nil, err
	}
	return &lambdamicrovms.DeleteMicrovmImageOutput{}, nil
}

func page(ids []string, next *string) *lambdamicrovms.ListMicrovmsOutput {
	out := &lambdamicrovms.ListMicrovmsOutput{NextToken: next}
	for _, id := range ids {
		out.Items = append(out.Items, types.MicrovmItem{
			MicrovmId: aws.String(id),
			State:     types.MicrovmStateRunning,
		})
	}
	return out
}

func TestTerminateAllFollowsThePageToken(t *testing.T) {
	token := "page-2"
	api := &fakeAPI{
		pages:     []*lambdamicrovms.ListMicrovmsOutput{page([]string{"mvm-a"}, &token), page([]string{"mvm-b"}, nil)},
		states:    map[string]types.MicrovmState{"mvm-a": types.MicrovmStateTerminated, "mvm-b": types.MicrovmStateTerminated},
		termErr:   map[string]error{},
		getErr:    map[string]error{},
		deleteErr: map[string]error{},
	}
	live, terminated, err := terminateAll(context.Background(), api)
	if err != nil {
		t.Fatalf("terminateAll: %v", err)
	}
	if api.listCalls != 2 {
		t.Errorf("listed %d times, want 2: the second page was never read", api.listCalls)
	}
	if len(live) != 2 {
		t.Errorf("found %d environments (%v), want 2", len(live), live)
	}
	if terminated != 2 {
		t.Errorf("terminated %d, want 2", terminated)
	}
}

func TestTerminateAllSkipsEnvironmentsAlreadyTerminated(t *testing.T) {
	api := &fakeAPI{
		pages: []*lambdamicrovms.ListMicrovmsOutput{page([]string{"mvm-a"}, nil)},
		states: map[string]types.MicrovmState{
			"mvm-a": types.MicrovmStateTerminated, "mvm-b": types.MicrovmStateTerminated,
		},
		termErr:   map[string]error{},
		getErr:    map[string]error{},
		deleteErr: map[string]error{},
	}
	api.pages[0].Items[0].State = types.MicrovmStateTerminated

	_, terminated, err := terminateAll(context.Background(), api)
	if err != nil {
		t.Fatalf("terminateAll: %v", err)
	}
	if terminated != 0 {
		t.Errorf("terminated %d, want 0 for an already-terminated environment", terminated)
	}
	if len(api.terminated) != 0 {
		t.Errorf("asked to terminate %v, want nothing", api.terminated)
	}
}

func TestTerminateAllDeduplicatesAcrossPages(t *testing.T) {
	token := "page-2"
	api := &fakeAPI{
		pages:     []*lambdamicrovms.ListMicrovmsOutput{page([]string{"mvm-a"}, &token), page([]string{"mvm-a"}, nil)},
		states:    map[string]types.MicrovmState{"mvm-a": types.MicrovmStateTerminated},
		termErr:   map[string]error{},
		getErr:    map[string]error{},
		deleteErr: map[string]error{},
	}
	live, terminated, err := terminateAll(context.Background(), api)
	if err != nil {
		t.Fatalf("terminateAll: %v", err)
	}
	if len(live) != 1 {
		t.Errorf("found %d environments, want 1", len(live))
	}
	if terminated != 1 {
		t.Errorf("terminated %d, want 1", terminated)
	}
}

func TestConfirmTerminatedFailsWhileAnEnvironmentStillRuns(t *testing.T) {
	api := &fakeAPI{states: map[string]types.MicrovmState{
		"mvm-a": types.MicrovmStateTerminated,
		"mvm-b": types.MicrovmStateRunning,
	}}
	err := confirmTerminated(context.Background(), api, []string{"mvm-a", "mvm-b"}, 30*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("a running environment was reported as cleared")
	}
	if !strings.Contains(err.Error(), "mvm-b") {
		t.Errorf("error %q does not name the survivor", err)
	}
}

func TestConfirmTerminatedDoesNotTreatAnUnreadableEnvironmentAsGone(t *testing.T) {
	api := &fakeAPI{
		states: map[string]types.MicrovmState{"mvm-a": types.MicrovmStateTerminated},
		getErr: map[string]error{"mvm-b": errors.New("expired token")},
	}
	err := confirmTerminated(context.Background(), api, []string{"mvm-a", "mvm-b"}, 30*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("an unreadable environment was reported as cleared")
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("error %q does not say the environment could not be read", err)
	}
}

func TestConfirmTerminatedPassesWhenEverythingIsTerminated(t *testing.T) {
	api := &fakeAPI{states: map[string]types.MicrovmState{
		"mvm-a": types.MicrovmStateTerminated, "mvm-b": types.MicrovmStateTerminated,
	}}
	if err := confirmTerminated(context.Background(), api, []string{"mvm-a", "mvm-b"}, time.Second, time.Millisecond); err != nil {
		t.Fatalf("confirmTerminated: %v", err)
	}
}

func TestConfirmTerminatedHonoursACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &fakeAPI{states: map[string]types.MicrovmState{"mvm-a": types.MicrovmStateRunning}}
	if err := confirmTerminated(ctx, api, []string{"mvm-a"}, time.Minute, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("confirmTerminated error = %v, want context.Canceled", err)
	}
}

func TestDeleteImagesReportsTheFailure(t *testing.T) {
	api := &fakeAPI{deleteErr: map[string]error{"arn:image": errors.New("in use")}}
	if _, err := deleteImages(context.Background(), api, []string{"arn:image"}); err == nil {
		t.Fatal("a failed image delete was reported as success")
	}
}

func TestTerminateAllReportsAListFailure(t *testing.T) {
	api := &fakeAPI{listErr: errors.New("access denied")}
	if _, _, err := terminateAll(context.Background(), api); err == nil {
		t.Fatal("a failed list was reported as an empty account")
	}
}
