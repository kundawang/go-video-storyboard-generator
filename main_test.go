package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fake FFmpeg harness
//
// The tests put a small script on PATH that stands in for ffmpeg and ffprobe.
// It records its arguments and creates deterministic output files, so tests can
// assert on the exact commands without decoding real video.
// ---------------------------------------------------------------------------

type fakeFFmpeg struct {
	binDir  string
	callLog string
	started string // marker written when a hang stage starts
}

func newFakeFFmpeg(t *testing.T) *fakeFFmpeg {
	t.Helper()
	dir := t.TempDir()
	f := &fakeFFmpeg{
		binDir:  dir,
		callLog: filepath.Join(dir, "calls.log"),
		started: filepath.Join(dir, "started"),
	}

	script := `#!/bin/sh
echo "ffmpeg $*" >> "` + f.callLog + `"
output=
prev=
for a in "$@"; do
  case "$a" in
    *.jpg|*.vtt) output="$a" ;;
  esac
  prev="$a"
done
` + f.stageScript() + `
`
	probe := `#!/bin/sh
echo "ffprobe $*" >> "` + f.callLog + `"
if [ -n "$FAKE_PROBE_FAIL" ]; then echo "boom" >&2; exit 1; fi
echo "${FAKE_DURATION:-30.0}"
`
	for name, body := range map[string]string{"ffmpeg": script, "ffprobe": probe} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_FRAME_COUNT", "7")
	t.Setenv("FAKE_STARTED", f.started)
	return f
}

// setFrames controls how many thumbnails the fake extractor writes.
func (f *fakeFFmpeg) setFrames(t *testing.T, n int) *fakeFFmpeg {
	t.Helper()
	t.Setenv("FAKE_FRAME_COUNT", strconv.Itoa(n))
	return f
}

// stageScript is embedded into the fake ffmpeg script. It decides whether this
// invocation is thumbnail extraction (pattern output) or tiling (tile filter).
func (f *fakeFFmpeg) stageScript() string {
	return `
is_tile=0
case "$*" in *tile=*) is_tile=1 ;; esac
if [ "$is_tile" = "1" ]; then
  if [ -n "$FAKE_FAIL_TILE" ]; then echo "tile failed" >&2; exit 1; fi
  if [ -n "$FAKE_HANG_TILE" ]; then : > "$FAKE_STARTED"; exec sleep 300; fi
  # Write a valid-looking JPEG placeholder; content is irrelevant to tests.
  printf '\377\330\377\331' > "$output"
  exit 0
fi
if [ -n "$FAKE_FAIL_EXTRACT" ]; then echo "extract failed" >&2; exit 1; fi
if [ -n "$FAKE_HANG_EXTRACT" ]; then : > "$FAKE_STARTED"; exec sleep 300; fi
i=1
while [ "$i" -le "${FAKE_FRAME_COUNT:-0}" ]; do
  printf '\377\330\377\331' > "$(dirname "$output")/$(printf 'thumb%05d.jpg' "$i")"
  i=$((i+1))
done
exit 0
`
}

func (f *fakeFFmpeg) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.callLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// runOptions builds a standalone config for tests that call lower-level code.
func testConfig(dir string, columns, rows int) config {
	return config{
		FPS:          1,
		Columns:      columns,
		Rows:         rows,
		TileWidth:    384,
		TileHeight:   160,
		OutputDir:    dir,
		BaseURL:      "https://example.test",
		ThumbnailDir: filepath.Join(dir, "thumbs"),
		InputFile:    "video.mp4",
	}
}

// parseVTT returns every cue in order, plus the cue text.
type parsedCue struct {
	start, end time.Duration
	image      string
	x, y, w, h int
}

func parseVTT(t *testing.T, content string) []parsedCue {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) < 1 || strings.TrimSpace(lines[0]) != "WEBVTT" {
		t.Fatalf("missing WEBVTT header in %q", content)
	}
	var cues []parsedCue
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.Contains(line, " --> ") {
			continue
		}
		parts := strings.Split(line, " --> ")
		if len(parts) != 2 {
			t.Fatalf("bad timing line %q", line)
		}
		start, err := parseVTTTime(parts[0])
		if err != nil {
			t.Fatalf("bad start %q: %v", parts[0], err)
		}
		end, err := parseVTTTime(parts[1])
		if err != nil {
			t.Fatalf("bad end %q: %v", parts[1], err)
		}
		if i+1 >= len(lines) {
			t.Fatalf("cue at line %d has no payload", i)
		}
		payload := strings.TrimSpace(lines[i+1])
		image, dims, ok := strings.Cut(payload, "#xywh=")
		if !ok {
			t.Fatalf("payload missing #xywh: %q", payload)
		}
		var x, y, w, h int
		if _, err := fmt.Sscanf(dims, "%d,%d,%d,%d", &x, &y, &w, &h); err != nil {
			t.Fatalf("bad xywh %q: %v", dims, err)
		}
		cues = append(cues, parsedCue{start, end, image, x, y, w, h})
		i++
	}
	return cues
}

func parseVTTTime(s string) (time.Duration, error) {
	var h, m int
	var sec float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%02d:%02d:%f", &h, &m, &sec); err != nil {
		return 0, err
	}
	return time.Duration(float64(h)*3600*float64(time.Second) +
		float64(m)*60*float64(time.Second) + sec*float64(time.Second)), nil
}

// ---------------------------------------------------------------------------
// Paging and cue tests
// ---------------------------------------------------------------------------

func TestBuildPagesContinuesTimeAcrossPages(t *testing.T) {
	cfg := testConfig(t.TempDir(), 3, 2) // capacity 6
	pages := buildPages(cfg, 14, 0)
	if len(pages) != 3 {
		t.Fatalf("got %d pages, want 3", len(pages))
	}
	if got := []int{pages[0].TileCount, pages[1].TileCount, pages[2].TileCount}; got[0] != 6 || got[1] != 6 || got[2] != 2 {
		t.Fatalf("tile counts = %v, want [6 6 2]", got)
	}

	var all []cue
	for _, p := range pages {
		all = append(all, p.Cues...)
	}
	if len(all) != 14 {
		t.Fatalf("got %d cues, want 14", len(all))
	}
	for i, c := range all {
		wantStart := time.Duration(i) * time.Second
		wantEnd := wantStart + time.Second
		if c.Start != wantStart || c.End != wantEnd {
			t.Errorf("cue %d = %v..%v, want %v..%v", i, c.Start, c.End, wantStart, wantEnd)
		}
		// Page two must continue at 6s, not restart.
		if i == 6 && c.Start != 6*time.Second {
			t.Errorf("first cue of page 2 starts at %v, want 6s", c.Start)
		}
	}
}

func TestBuildPagesTwoHourRecording(t *testing.T) {
	// Two hours at 1 FPS is 7200 thumbnails; with a 10x10 grid that is 72 pages.
	cfg := testConfig(t.TempDir(), 10, 10)
	pages := buildPages(cfg, 7200, 2*time.Hour)
	if len(pages) != 72 {
		t.Fatalf("got %d pages, want 72", len(pages))
	}
	for i, p := range pages {
		if p.TileCount != 100 {
			t.Fatalf("page %d has %d tiles, want 100", i+1, p.TileCount)
		}
		if p.Grid.Columns != 10 || p.Grid.Rows != 10 {
			t.Fatalf("page %d grid = %+v, want 10x10", i+1, p.Grid)
		}
		if got, want := p.ImageName, imageName(i); got != want {
			t.Fatalf("page %d image = %q, want %q", i+1, got, want)
		}
	}
	last := pages[len(pages)-1]
	finalCue := last.Cues[len(last.Cues)-1]
	if finalCue.Start != 7199*time.Second || finalCue.End != 2*time.Hour {
		t.Fatalf("final cue = %v..%v, want 7199s..2h", finalCue.Start, finalCue.End)
	}
}

func TestBuildPagesCoordinatesMatchPageLayout(t *testing.T) {
	cfg := testConfig(t.TempDir(), 3, 2)
	pages := buildPages(cfg, 8, 0) // page 1: 6 tiles, page 2: 2 tiles

	// Page 1 uses a 3x2 grid.
	wantPage1 := []struct{ x, y int }{
		{0, 0}, {384, 0}, {768, 0},
		{0, 160}, {384, 160}, {768, 160},
	}
	for i, want := range wantPage1 {
		got := pages[0].Cues[i]
		if got.X != want.x || got.Y != want.y {
			t.Errorf("page 1 cue %d xy = (%d,%d), want (%d,%d)", i, got.X, got.Y, want.x, want.y)
		}
	}

	// Page 2 holds only two tiles; coordinates must restart at the origin.
	for i, c := range pages[1].Cues {
		wantX := i * 384
		if c.X != wantX || c.Y != 0 {
			t.Errorf("page 2 cue %d xy = (%d,%d), want (%d,0)", i, c.X, c.Y, wantX)
		}
	}
}

func TestPageLayoutAvoidsBlankCells(t *testing.T) {
	cfg := testConfig(t.TempDir(), 10, 10)
	if got := pageLayout(cfg, 100); got.Columns != 10 || got.Rows != 10 {
		t.Fatalf("full page layout = %+v, want 10x10", got)
	}
	if got := pageLayout(cfg, 7); got.Columns != 7 || got.Rows != 1 {
		t.Fatalf("partial layout = %+v, want 7x1", got)
	}
	if got := pageLayout(cfg, 21); got.Columns != 7 || got.Rows != 3 {
		t.Fatalf("partial layout = %+v, want 7x3 (no blank cells)", got)
	}
}

func TestWriteVTTUsesPerPageImage(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, 2, 2)
	pages := buildPages(cfg, 5, 0) // 4 tiles on page 1, 1 on page 2
	path := filepath.Join(dir, "storyboard.vtt")
	if err := writeVTT(cfg, pages, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cues := parseVTT(t, string(data))
	if len(cues) != 5 {
		t.Fatalf("got %d cues, want 5", len(cues))
	}

	wantImages := []string{
		"https://example.test/storyboard.jpg",
		"https://example.test/storyboard.jpg",
		"https://example.test/storyboard.jpg",
		"https://example.test/storyboard.jpg",
		"https://example.test/storyboard-2.jpg",
	}
	wantXY := [][2]int{{0, 0}, {384, 0}, {0, 160}, {384, 160}, {0, 0}}
	for i, c := range cues {
		if c.image != wantImages[i] {
			t.Errorf("cue %d image = %q, want %q", i, c.image, wantImages[i])
		}
		if c.x != wantXY[i][0] || c.y != wantXY[i][1] {
			t.Errorf("cue %d xy = (%d,%d), want %v", i, c.x, c.y, wantXY[i])
		}
		if c.w != 384 || c.h != 160 {
			t.Errorf("cue %d size = %dx%d, want 384x160", i, c.w, c.h)
		}
	}

	// Times must be strictly increasing across the page boundary.
	for i := 1; i < len(cues); i++ {
		if cues[i].start != cues[i-1].end {
			t.Errorf("cue %d starts at %v but previous ended at %v", i, cues[i].start, cues[i-1].end)
		}
	}
}

func TestSinglePageKeepsOriginalFilename(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir, 10, 10)
	pages := buildPages(cfg, 6, 0)
	if len(pages) != 1 {
		t.Fatalf("got %d pages, want 1", len(pages))
	}
	if pages[0].ImageName != "storyboard.jpg" {
		t.Fatalf("image name = %q, want storyboard.jpg", pages[0].ImageName)
	}
}

func TestCueEndClampedToKnownDuration(t *testing.T) {
	cfg := testConfig(t.TempDir(), 10, 10)
	pages := buildPages(cfg, 3, 2500*time.Millisecond)
	last := pages[0].Cues[2]
	if last.End != 2500*time.Millisecond {
		t.Fatalf("last cue end = %v, want 2.5s", last.End)
	}
	if last.Start != 2*time.Second {
		t.Fatalf("last cue start = %v, want 2s", last.Start)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                       "00:00:00.000",
		1500 * time.Millisecond: "00:00:01.500",
		2*time.Hour + 3*time.Minute + 4*time.Second + 5*time.Millisecond: "02:03:04.005",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// End-to-end command flow with the fake FFmpeg
// ---------------------------------------------------------------------------

func TestRunPagedEndToEndWithFakeFFmpeg(t *testing.T) {
	fake := newFakeFFmpeg(t)
	fake.setFrames(t, 14)

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	opts := options{
		fps:       1,
		columns:   3,
		rows:      2,
		width:     384,
		height:    160,
		baseURL:   "https://cdn.test",
		inputFile: filepath.Join(dir, "video.mp4"),
		outputDir: out,
	}
	if err := os.WriteFile(opts.inputFile, []byte("not a real video"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, name := range []string{"storyboard.jpg", "storyboard-2.jpg", "storyboard-3.jpg", "storyboard.vtt"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing output %s: %v", name, err)
		}
	}
	// No temporary files or thumbnail directory may survive a successful run.
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temporary artifact left behind: %s", e.Name())
		}
	}

	data, err := os.ReadFile(filepath.Join(out, "storyboard.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	cues := parseVTT(t, string(data))
	if len(cues) != 14 {
		t.Fatalf("got %d cues, want 14", len(cues))
	}
	if !strings.HasPrefix(cues[6].image, "https://cdn.test/storyboard-2.jpg") {
		t.Errorf("cross-page cue points at %q", cues[6].image)
	}
	if cues[6].start != 6*time.Second {
		t.Errorf("cross-page cue starts at %v, want 6s", cues[6].start)
	}
	if cues[13].image != "https://cdn.test/storyboard-3.jpg" || cues[13].x != 384 || cues[13].y != 0 {
		t.Errorf("final cue = %+v", cues[13])
	}

	// The tiling commands must fetch the right thumbnail run for each page.
	var tileCalls []string
	for _, call := range fake.calls(t) {
		if strings.Contains(call, "tile=") {
			tileCalls = append(tileCalls, call)
		}
	}
	if len(tileCalls) != 3 {
		t.Fatalf("got %d tile calls, want 3: %v", len(tileCalls), tileCalls)
	}
	wantStart := []string{"-start_number 1", "-start_number 7", "-start_number 13"}
	for i, want := range wantStart {
		if !strings.Contains(tileCalls[i], want) {
			t.Errorf("tile call %d = %q, want %q", i, tileCalls[i], want)
		}
	}
	if !strings.Contains(tileCalls[2], "tile=2x1") {
		t.Errorf("final page tile grid = %q, want 2x1", tileCalls[2])
	}
}

func TestRunUnknownDurationStillProducesCues(t *testing.T) {
	newFakeFFmpeg(t)
	t.Setenv("FAKE_DURATION", "N/A")
	t.Setenv("FAKE_PROBE_FAIL", "1")

	out := t.TempDir()
	opts := options{
		fps: 1, columns: 2, rows: 2, width: 384, height: 160,
		baseURL: "https://cdn.test", inputFile: "video.mp4", outputDir: out,
	}
	if err := run(context.Background(), opts); err != nil {
		t.Fatalf("unknown duration must not abort by default: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "storyboard.vtt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(parseVTT(t, string(data))); got != 7 {
		t.Fatalf("got %d cues, want 7", got)
	}
}

func TestRunUnknownDurationRequireDurationFails(t *testing.T) {
	newFakeFFmpeg(t)
	t.Setenv("FAKE_PROBE_FAIL", "1")

	out := t.TempDir()
	opts := options{
		fps: 1, columns: 2, rows: 2, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out, requireDuration: true,
	}
	err := run(context.Background(), opts)
	if !errors.Is(err, ErrUnknownDuration) {
		t.Fatalf("err = %v, want ErrUnknownDuration", err)
	}
	assertNoArtifacts(t, out)
}

func TestRunNoFramesFails(t *testing.T) {
	fake := newFakeFFmpeg(t)
	fake.setFrames(t, 0)

	out := t.TempDir()
	opts := options{
		fps: 1, columns: 2, rows: 2, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out,
	}
	err := run(context.Background(), opts)
	if !errors.Is(err, ErrNoFrames) {
		t.Fatalf("err = %v, want ErrNoFrames", err)
	}
	assertNoArtifacts(t, out)
}

func TestRunExtractionFailureCleansUp(t *testing.T) {
	newFakeFFmpeg(t)
	t.Setenv("FAKE_FAIL_EXTRACT", "1")

	out := t.TempDir()
	opts := options{
		fps: 1, columns: 2, rows: 2, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out,
	}
	if err := run(context.Background(), opts); err == nil {
		t.Fatal("expected extraction failure")
	}
	assertNoArtifacts(t, out)
}

func TestRunTilingFailureCleansUp(t *testing.T) {
	newFakeFFmpeg(t)
	t.Setenv("FAKE_FAIL_TILE", "1")

	out := t.TempDir()
	opts := options{
		fps: 1, columns: 2, rows: 2, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out,
	}
	if err := run(context.Background(), opts); err == nil {
		t.Fatal("expected tiling failure")
	}
	assertNoArtifacts(t, out)
}

func TestRunRefusesExistingOutputWithoutForce(t *testing.T) {
	newFakeFFmpeg(t)
	out := t.TempDir()
	existing := filepath.Join(out, "storyboard.jpg")
	if err := os.WriteFile(existing, []byte("previous"), 0644); err != nil {
		t.Fatal(err)
	}

	opts := options{
		fps: 1, columns: 10, rows: 10, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out,
	}
	err := run(context.Background(), opts)
	if !errors.Is(err, ErrOutputExists) {
		t.Fatalf("err = %v, want ErrOutputExists", err)
	}
	// The pre-existing file must be untouched.
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "previous" {
		t.Fatalf("existing output was modified: %q %v", data, err)
	}
}

func TestRunForceOverwritesExistingOutput(t *testing.T) {
	newFakeFFmpeg(t)
	out := t.TempDir()
	existing := filepath.Join(out, "storyboard.jpg")
	if err := os.WriteFile(existing, []byte("previous"), 0644); err != nil {
		t.Fatal(err)
	}

	opts := options{
		fps: 1, columns: 10, rows: 10, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out, force: true,
	}
	if err := run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	data, _ := os.ReadFile(existing)
	if string(data) == "previous" {
		t.Error("force did not overwrite the existing output")
	}
}

func TestRunCanceledDuringExtractionCleansUp(t *testing.T) {
	fake := newFakeFFmpeg(t)
	t.Setenv("FAKE_HANG_EXTRACT", "1")

	out := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	opts := options{
		fps: 1, columns: 2, rows: 2, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out,
	}
	go func() { done <- run(ctx, opts) }()

	waitForFile(t, fake.started)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertNoArtifacts(t, out)
}

func TestProbeDurationFallsBackToStreamDuration(t *testing.T) {
	dir := t.TempDir()
	// Mimic a container whose format duration is N/A but whose stream duration
	// is usable: two calls, first unusable, second valid.
	counter := filepath.Join(dir, "count")
	script := `#!/bin/sh
n=$(cat "` + counter + `" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "` + counter + `"
if [ "$n" -eq 1 ]; then echo "N/A"; else echo "12.5"; fi
`
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := probeDuration(context.Background(), "video.mp4")
	if err != nil {
		t.Fatalf("probeDuration: %v", err)
	}
	if want := 12500 * time.Millisecond; got != want {
		t.Fatalf("duration = %v, want %v", got, want)
	}
}

func TestProbeDurationNoUsableValue(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho N/A\n"
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := probeDuration(context.Background(), "video.mp4")
	if !errors.Is(err, ErrUnknownDuration) {
		t.Fatalf("err = %v, want ErrUnknownDuration", err)
	}
}

func TestRunForceRemovesStalePages(t *testing.T) {
	fake := newFakeFFmpeg(t)
	fake.setFrames(t, 3) // fits on one page with a 10x10 grid

	out := t.TempDir()
	// Simulate a previous run that produced three pages.
	for _, name := range []string{"storyboard.jpg", "storyboard-2.jpg", "storyboard-3.jpg", "storyboard.vtt"} {
		if err := os.WriteFile(filepath.Join(out, name), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	opts := options{
		fps: 1, columns: 10, rows: 10, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out, force: true,
	}
	if err := run(context.Background(), opts); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, stale := range []string{"storyboard-2.jpg", "storyboard-3.jpg"} {
		if _, err := os.Stat(filepath.Join(out, stale)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("stale page %s still present", stale)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "storyboard.jpg")); err != nil {
		t.Errorf("first page missing: %v", err)
	}
}

func TestProbeDurationParsesValue(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 42.25\n"
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := probeDuration(context.Background(), "video.mp4")
	if err != nil {
		t.Fatalf("probeDuration: %v", err)
	}
	if want := 42250 * time.Millisecond; got != want {
		t.Fatalf("duration = %v, want %v", got, want)
	}
}

func TestRunCanceledDuringTilingCleansUp(t *testing.T) {
	fake := newFakeFFmpeg(t)
	t.Setenv("FAKE_HANG_TILE", "1")

	out := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	opts := options{
		fps: 1, columns: 2, rows: 2, width: 384, height: 160,
		inputFile: "video.mp4", outputDir: out,
	}
	go func() { done <- run(ctx, opts) }()

	waitForFile(t, fake.started)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertNoArtifacts(t, out)
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// assertNoArtifacts verifies a failed run leaves neither outputs nor temp files.
func assertNoArtifacts(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("leftover artifact: %s", e.Name())
	}
}

// ---------------------------------------------------------------------------
// Original API compatibility
// ---------------------------------------------------------------------------

func TestOriginalHelpersStillExist(t *testing.T) {
	// Compile-time checks that the first version's functions remain available.
	var _ = showUsage
	var _ = parseArguments
	var _ = ensureDirectoryExists
	var _ = generateThumbnails
	var _ = generateStoryboard
	var _ = generateVTT
	var _ = cleanup
	var _ = exec.Command
}
