package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the segmented update flow.
//
// The update runs in three phases - the metadata check, the confirmation of
// the user and the download of the installer - and every phase has its own
// budget. These tests prove that the phases do not share a deadline.

// updateRecorder collects what the update flow did.
type updateRecorder struct {
	mu sync.Mutex

	checkCalls    int
	downloadCalls int
	launchCalls   int

	// checkDeadline is the deadline of the metadata check.
	checkDeadline time.Time
	// downloadDeadline is the deadline of the download.
	downloadDeadline time.Time
	// checkHasDeadline tells a deadline apart from a context without one.
	checkHasDeadline bool
	// downloadHasDeadline tells a deadline apart from a context without one.
	downloadHasDeadline bool
	// downloadErr is returned by the download.
	downloadErr error
	// messages holds everything the flow reported to the user.
	messages []string
}

// testFlow builds a flow whose phases are recorded instead of executed.
func testFlow(rec *updateRecorder, release *UpdateRelease, confirm func() bool) *updateFlow {
	return &updateFlow{
		check: func(ctx context.Context) (*UpdateRelease, error) {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.checkCalls++
			deadline, ok := ctx.Deadline()
			rec.checkDeadline = deadline
			rec.checkHasDeadline = ok
			return release, nil
		},
		download: func(ctx context.Context, rel *UpdateRelease) (string, func(), error) {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.downloadCalls++
			deadline, ok := ctx.Deadline()
			rec.downloadDeadline = deadline
			rec.downloadHasDeadline = ok
			if rec.downloadErr != nil {
				return "", func() {}, rec.downloadErr
			}
			return `C:\staging\TaskView-Setup.exe`, func() {}, nil
		},
		confirm: func(title, message string) bool {
			if confirm == nil {
				return true
			}
			return confirm()
		},
		notify: func(title, message string) {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.messages = append(rec.messages, message)
		},
		launch: func(ctx context.Context, filename string) error {
			rec.mu.Lock()
			defer rec.mu.Unlock()
			rec.launchCalls++
			return nil
		},
		legacyPending:  func() (bool, error) { return false, nil },
		currentVersion: "1.2.3",
	}
}

// The update check and the download must not share one deadline: a user who
// leaves the confirmation prompt open must not make the download fail with
// "context deadline exceeded" before it even starts.
func TestUpdateFlowGivesEveryPhaseItsOwnBudget(t *testing.T) {
	rec := &updateRecorder{}
	release := &UpdateRelease{Version: "v1.3.0"}
	flow := testFlow(rec, release, nil)

	before := time.Now()
	if err := flow.run(context.Background()); err != nil {
		t.Fatalf("the update failed: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.checkCalls != 1 || rec.downloadCalls != 1 || rec.launchCalls != 1 {
		t.Fatalf("check=%d download=%d launch=%d, want one call each", rec.checkCalls, rec.downloadCalls, rec.launchCalls)
	}
	if !rec.checkHasDeadline {
		t.Fatal("the metadata check must run under a deadline of its own")
	}
	if !rec.downloadHasDeadline {
		t.Fatal("the download must run under a deadline of its own")
	}

	// The metadata check is bounded by the short metadata timeout.
	if remaining := rec.checkDeadline.Sub(before); remaining > updateMetadataTimeout {
		t.Errorf("the metadata check may run for %s, want at most %s", remaining, updateMetadataTimeout)
	}
	// The download gets the full download budget, no matter how much of the
	// metadata budget was used.
	if remaining := rec.downloadDeadline.Sub(before); remaining > updateDownloadTimeout {
		t.Errorf("the download may run for %s, want at most %s", remaining, updateDownloadTimeout)
	}
	if !rec.downloadDeadline.After(rec.checkDeadline) {
		t.Errorf("the download deadline %s is not after the metadata deadline %s", rec.downloadDeadline, rec.checkDeadline)
	}
}

// withUpdateTimeout temporarily lowers one of the phase budgets.
func withUpdateTimeout(t *testing.T, target *time.Duration, value time.Duration) {
	t.Helper()
	original := *target
	*target = value
	t.Cleanup(func() { *target = original })
}

// The confirmation is the user's time: it may take longer than the whole
// metadata budget, and the download must still run with a full budget.
func TestUpdateFlowKeepsTheDownloadBudgetAcrossASlowConfirmation(t *testing.T) {
	// The metadata budget is lowered so the test does not have to wait for it.
	withUpdateTimeout(t, &updateMetadataTimeout, 50*time.Millisecond)

	rec := &updateRecorder{}
	release := &UpdateRelease{Version: "v1.3.0"}

	// The metadata check is quick, the confirmation takes much longer than
	// the metadata budget.
	confirmStarted := time.Now()
	flow := testFlow(rec, release, func() bool {
		time.Sleep(updateMetadataTimeout + 50*time.Millisecond)
		return true
	})

	before := time.Now()
	if err := flow.run(context.Background()); err != nil {
		t.Fatalf("the update failed: %v", err)
	}
	elapsed := time.Since(before)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 1 {
		t.Fatalf("the download ran %d times, want once", rec.downloadCalls)
	}
	// The download deadline must still be a full download budget away: the
	// slow confirmation did not eat into it.
	if remaining := rec.downloadDeadline.Sub(confirmStarted); remaining <= updateMetadataTimeout {
		t.Errorf("the download budget is %s, want more than the metadata timeout %s", remaining, updateMetadataTimeout)
	}
	if remaining := rec.downloadDeadline.Sub(time.Now()); remaining <= 0 {
		t.Errorf("the download deadline %s is already in the past", rec.downloadDeadline)
	}
	if elapsed < updateMetadataTimeout {
		t.Errorf("the confirmation only took %s, the test did not exercise a slow answer", elapsed)
	}
}

// A slow confirmation must not turn into a failed download either: the
// download runs under a context that is not expired yet.
func TestUpdateFlowDownloadsAfterASlowConfirmation(t *testing.T) {
	rec := &updateRecorder{}
	release := &UpdateRelease{Version: "v1.3.0"}

	flow := testFlow(rec, release, func() bool {
		time.Sleep(200 * time.Millisecond)
		return true
	})

	if err := flow.run(context.Background()); err != nil {
		t.Fatalf("the update failed: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 1 || rec.launchCalls != 1 {
		t.Fatalf("download=%d launch=%d, want one call each", rec.downloadCalls, rec.launchCalls)
	}
}

// A user who says no must not trigger a download.
func TestUpdateFlowSkipsTheDownloadWhenTheUserDeclines(t *testing.T) {
	rec := &updateRecorder{}
	release := &UpdateRelease{Version: "v1.3.0"}
	flow := testFlow(rec, release, func() bool { return false })

	if err := flow.run(context.Background()); err != nil {
		t.Fatalf("the update failed: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 0 || rec.launchCalls != 0 {
		t.Fatalf("download=%d launch=%d, want none after the user declined", rec.downloadCalls, rec.launchCalls)
	}
}

// A metadata check that runs into its deadline must fail with the timeout,
// and it must not start a download.
func TestUpdateFlowStopsWhenTheMetadataCheckTimesOut(t *testing.T) {
	// The metadata budget is lowered so the test does not have to wait for it.
	withUpdateTimeout(t, &updateMetadataTimeout, 50*time.Millisecond)

	rec := &updateRecorder{}
	release := &UpdateRelease{Version: "v1.3.0"}
	flow := testFlow(rec, release, nil)
	flow.check = func(ctx context.Context) (*UpdateRelease, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	if err := flow.run(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the update failed with %v, want a deadline error", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 0 {
		t.Fatalf("the download ran %d times after a failed check, want none", rec.downloadCalls)
	}
}

// The shutdown of the application ends the flow: a confirmation that is
// answered after the shutdown must not start a download.
func TestUpdateFlowStopsWhenTheApplicationShutsDown(t *testing.T) {
	rec := &updateRecorder{}
	release := &UpdateRelease{Version: "v1.3.0"}

	ctx, cancel := context.WithCancel(context.Background())
	flow := testFlow(rec, release, func() bool {
		cancel()
		return true
	})

	if err := flow.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the update failed with %v, want a cancellation error", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 0 {
		t.Fatalf("the download ran %d times after the shutdown, want none", rec.downloadCalls)
	}
}

// A download that fails must not start the installer.
func TestUpdateFlowKeepsTheInstallerAwayWhenTheDownloadFails(t *testing.T) {
	rec := &updateRecorder{downloadErr: errors.New("network down")}
	release := &UpdateRelease{Version: "v1.3.0"}
	flow := testFlow(rec, release, nil)

	err := flow.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("the update failed with %v, want the download error", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.launchCalls != 0 {
		t.Fatalf("the installer was started %d times after a failed download, want none", rec.launchCalls)
	}
}

// A build that is already up to date must say so and download nothing.
func TestUpdateFlowReportsAnUpToDateBuild(t *testing.T) {
	rec := &updateRecorder{}
	flow := testFlow(rec, nil, nil)

	if err := flow.run(context.Background()); err != nil {
		t.Fatalf("the update failed: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 0 {
		t.Fatalf("the download ran %d times for an up to date build, want none", rec.downloadCalls)
	}
	if len(rec.messages) == 0 || !strings.Contains(rec.messages[0], "最新版本") {
		t.Fatalf("the user was told %v, want the up to date message", rec.messages)
	}
}

// A repository without a published release must say so and download nothing.
func TestUpdateFlowReportsAMissingRelease(t *testing.T) {
	rec := &updateRecorder{}
	flow := testFlow(rec, nil, nil)
	flow.check = func(ctx context.Context) (*UpdateRelease, error) { return nil, ErrNoRelease }

	if err := flow.run(context.Background()); err != nil {
		t.Fatalf("the update failed: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 0 {
		t.Fatalf("the download ran %d times without a release, want none", rec.downloadCalls)
	}
	if len(rec.messages) == 0 || !strings.Contains(rec.messages[0], "暂无正式发布版本") {
		t.Fatalf("the user was told %v, want the missing release message", rec.messages)
	}
}

// The whole flow with the real updater: a confirmation that takes longer
// than the metadata budget must not stop the download of the installer.
//
// This is the case the single shared deadline used to break: the prompt
// stayed open, the deadline ran out and the download failed with
// "context deadline exceeded".
func TestUpdateFlowDownloadsTheRealInstallerAfterASlowConfirmation(t *testing.T) {
	// The metadata budget is lowered so the test does not have to wait for it.
	withUpdateTimeout(t, &updateMetadataTimeout, 50*time.Millisecond)

	payload := []byte("this is the installer")
	files := map[string][]byte{
		assetPath(testTag, testInstallerName): payload,
		assetPath(testTag, checksumAssetName): []byte(checksumLine(payload, testInstallerName)),
	}
	github := newFakeGitHubServer(t, standardRelease(testTag), files)
	updater := &Updater{Client: github.client, CurrentVersion: "v1.2.3", APIURL: defaultAPIURL}

	before := stagingDirs(t)
	rec := &updateRecorder{}
	flow := testFlow(rec, nil, func() bool {
		// The confirmation takes longer than the metadata budget.
		time.Sleep(updateMetadataTimeout + 50*time.Millisecond)
		return true
	})
	flow.check = updater.Check

	var installerPath string
	flow.download = func(ctx context.Context, rel *UpdateRelease) (string, func(), error) {
		rec.mu.Lock()
		rec.downloadCalls++
		rec.mu.Unlock()
		path, clean, err := updater.Download(ctx, rel)
		installerPath = path
		return path, clean, err
	}
	// The real flow leaves the staging directory alone: the verified
	// installer must outlive this process. The test removes it instead.
	flow.launch = func(ctx context.Context, filename string) error {
		rec.mu.Lock()
		rec.launchCalls++
		rec.mu.Unlock()
		return os.RemoveAll(filepath.Dir(filename))
	}

	if err := flow.run(context.Background()); err != nil {
		t.Fatalf("the update failed: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.downloadCalls != 1 || rec.launchCalls != 1 {
		t.Fatalf("download=%d launch=%d, want one call each", rec.downloadCalls, rec.launchCalls)
	}
	if installerPath == "" {
		t.Fatal("the installer was not downloaded")
	}
	if after := stagingDirs(t); len(after) != len(before) {
		t.Fatalf("expected no leftover staging directories, before %v after %v", before, after)
	}
}
