package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultColumns       = 10
	defaultRows          = 10
	defaultThumbnailW    = 384
	defaultThumbnailH    = 160
	defaultStoryboardURL = "https://insertlinkhere.com"

	thumbnailNamePattern = "thumb%05d.jpg"
)

var (
	// ErrNoFrames means FFmpeg completed but produced no thumbnail at all.
	ErrNoFrames = errors.New("no frames were extracted from the video")
	// ErrUnknownDuration means the video duration could not be determined.
	ErrUnknownDuration = errors.New("unable to determine video duration")
	// ErrOutputExists means an output file would be overwritten. Pass -force to
	// replace the previous storyboard explicitly.
	ErrOutputExists = errors.New("output file already exists")
)

// config describes one storyboard run. Columns*Rows is the maximum number of
// thumbnails a single image may contain; larger videos are split across pages.
type config struct {
	FPS          int // sample one frame every FPS seconds
	Columns      int
	Rows         int
	TileWidth    int
	TileHeight   int
	OutputDir    string
	BaseURL      string
	Force        bool
	ThumbnailDir string // temporary directory, removed when the run ends
	InputFile    string
}

// cue is one WebVTT timing entry pointing at a tile rectangle.
type cue struct {
	Start time.Duration
	End   time.Duration
	X     int
	Y     int
}

// layout is the concrete grid used by one page. For the final page it may be
// narrower than the configured maximum so that blank cells are avoided.
type layout struct {
	Columns int
	Rows    int
}

// String returns the value expected by FFmpeg's tile filter.
func (l layout) String() string {
	return fmt.Sprintf("%dx%d", l.Columns, l.Rows)
}

// page is one storyboard image and the cues whose tiles live in it.
type page struct {
	FirstTile int // index of the first thumbnail on this page
	TileCount int
	Grid      layout
	Cues      []cue
	ImageName string // file name, e.g. storyboard.jpg or storyboard-2.jpg
}

type options struct {
	fps             int
	columns         int
	rows            int
	width           int
	height          int
	baseURL         string
	force           bool
	requireDuration bool
	duration        float64
	inputFile       string
	outputDir       string
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		printUsage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startTime := time.Now()
	if err := run(ctx, opts); err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(os.Stderr, "Canceled: partial storyboard output was removed.")
			os.Exit(130)
		default:
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("Process completed in %v.\n", time.Since(startTime))
}

func parseOptions(args []string) (options, error) {
	opts := options{}
	fs := flag.NewFlagSet("storyboard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&opts.columns, "columns", defaultColumns, "maximum thumbnails per row")
	fs.IntVar(&opts.rows, "rows", defaultRows, "maximum thumbnails per column")
	fs.IntVar(&opts.width, "width", defaultThumbnailW, "width of one thumbnail in pixels")
	fs.IntVar(&opts.height, "height", defaultThumbnailH, "height of one thumbnail in pixels")
	fs.StringVar(&opts.baseURL, "base-url", defaultStoryboardURL, "base URL used in VTT image links")
	fs.BoolVar(&opts.force, "force", false, "overwrite existing storyboard output files")
	fs.BoolVar(&opts.requireDuration, "require-duration", false, "fail when the video duration is unknown")
	fs.Float64Var(&opts.duration, "duration", 0, "video duration in seconds; skips probing when positive")

	if err := fs.Parse(args); err != nil {
		return opts, err
	}

	rest := fs.Args()
	if len(rest) != 3 {
		return opts, errors.New("expected <fps> <input_file> <output_dir>")
	}
	fps, err := strconv.Atoi(rest[0])
	if err != nil || fps <= 0 {
		return opts, errors.New("invalid FPS: provide a positive integer")
	}
	opts.fps = fps
	opts.inputFile = rest[1]
	opts.outputDir = rest[2]

	if opts.columns <= 0 || opts.rows <= 0 {
		return opts, errors.New("columns and rows must be positive")
	}
	if opts.width <= 0 || opts.height <= 0 {
		return opts, errors.New("thumbnail width and height must be positive")
	}
	if opts.duration < 0 || math.IsNaN(opts.duration) || math.IsInf(opts.duration, 0) {
		return opts, errors.New("duration must be a positive number of seconds")
	}
	return opts, nil
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage: storyboard [options] <fps> <input_file> <output_dir>")
	fmt.Fprintln(os.Stderr, "  -columns int      maximum thumbnails per row (default 10)")
	fmt.Fprintln(os.Stderr, "  -rows int         maximum thumbnails per column (default 10)")
	fmt.Fprintln(os.Stderr, "  -width int        thumbnail width (default 384)")
	fmt.Fprintln(os.Stderr, "  -height int       thumbnail height (default 160)")
	fmt.Fprintln(os.Stderr, "  -base-url string  base URL used in VTT image links")
	fmt.Fprintln(os.Stderr, "  -force            overwrite existing storyboard output")
	fmt.Fprintln(os.Stderr, "  -duration float   video duration in seconds (skips probing)")
	fmt.Fprintln(os.Stderr, "  -require-duration fail if the video duration cannot be determined")
	fmt.Fprintln(os.Stderr, "Example: storyboard -columns 10 -rows 10 1 example.mp4 ./output")
}

// run performs the whole job. Storyboard pages are assembled into temporary
// files and only renamed into place once every page and the VTT succeeded, so
// a failure or interruption never leaves a mixed or half-written output set.
func run(ctx context.Context, opts options) error {
	if err := os.MkdirAll(opts.outputDir, os.ModePerm); err != nil {
		return fmt.Errorf("creating output directory %s: %w", opts.outputDir, err)
	}

	duration, known, err := resolveDuration(ctx, opts)
	if err != nil {
		return err
	}
	if !known {
		fmt.Fprintln(os.Stderr, "Warning: video duration is unknown; cue times are based on the sampling interval.")
	}

	thumbDir, err := os.MkdirTemp(opts.outputDir, ".storyboard-thumbs-")
	if err != nil {
		return fmt.Errorf("creating temporary thumbnail directory: %w", err)
	}
	defer os.RemoveAll(thumbDir)

	cfg := config{
		FPS:          opts.fps,
		Columns:      opts.columns,
		Rows:         opts.rows,
		TileWidth:    opts.width,
		TileHeight:   opts.height,
		OutputDir:    opts.outputDir,
		BaseURL:      strings.TrimRight(opts.baseURL, "/"),
		Force:        opts.force,
		ThumbnailDir: thumbDir,
		InputFile:    opts.inputFile,
	}

	// Fail fast on the files every run writes, before spending time extracting
	// thumbnails from a long video. Page-specific conflicts are re-checked once
	// the page count is known.
	if err := checkBaseOutputConflicts(cfg); err != nil {
		return err
	}

	thumbnailCount, err := extractThumbnails(ctx, cfg)
	if err != nil {
		return err
	}

	pages := buildPages(cfg, thumbnailCount, duration)
	if err := checkOutputConflicts(cfg, pages); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	temps := make([]string, 0, len(pages)+1)
	ok := false
	defer func() {
		if !ok {
			removeFiles(temps...)
		}
	}()

	for _, p := range pages {
		tmp, err := assemblePage(ctx, cfg, p)
		if err != nil {
			return err
		}
		temps = append(temps, tmp)
	}

	vttTmp, err := writeVTTTemp(ctx, cfg, pages)
	if err != nil {
		return err
	}
	temps = append(temps, vttTmp)

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := commitOutputs(cfg, pages, temps); err != nil {
		return err
	}
	if cfg.Force {
		removeStalePages(cfg, len(pages))
	}
	ok = true

	fmt.Printf("Created %d storyboard page(s).\n", len(pages))
	return nil
}

// resolveDuration returns the video duration when it can be determined. An
// unknown duration is not fatal by default, because cue timing only needs the
// sampling interval; -require-duration makes it an explicit failure instead.
func resolveDuration(ctx context.Context, opts options) (time.Duration, bool, error) {
	if opts.duration > 0 {
		return time.Duration(opts.duration * float64(time.Second)), true, nil
	}
	duration, err := probeDuration(ctx, opts.inputFile)
	if err == nil {
		return duration, true, nil
	}
	if ctx.Err() != nil {
		return 0, false, ctx.Err()
	}
	if opts.requireDuration {
		return 0, false, err
	}
	return 0, false, nil
}

// probeDuration asks ffprobe for the media duration. Container duration is
// preferred, with per-stream duration as a fallback for containers that only
// report it there. A missing ffprobe, a malformed answer, or "N/A" is reported
// as ErrUnknownDuration.
func probeDuration(ctx context.Context, inputFile string) (time.Duration, error) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, fmt.Errorf("%w: ffprobe is not available: %v", ErrUnknownDuration, err)
	}

	// Container duration is authoritative; some formats only expose it on the
	// stream, so fall back to the stream duration.
	for _, entries := range []string{"format=duration", "stream=duration"} {
		cmd := exec.CommandContext(ctx, ffprobe,
			"-v", "error",
			"-show_entries", entries,
			"-of", "default=noprint_wrappers=1:nokey=1",
			inputFile,
		)
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("%w: %v", ErrUnknownDuration, err)
		}
		if d, ok := firstPositiveDuration(string(out)); ok {
			return d, nil
		}
	}
	return 0, fmt.Errorf("%w: no usable duration reported", ErrUnknownDuration)
}

// firstPositiveDuration parses ffprobe's line-per-value output, taking the
// first finite positive value. "N/A" and malformed lines are skipped.
func firstPositiveDuration(out string) (time.Duration, bool) {
	for _, raw := range strings.Split(out, "\n") {
		value := strings.TrimSpace(raw)
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
			continue
		}
		return time.Duration(seconds * float64(time.Second)), true
	}
	return 0, false
}

// extractThumbnails samples one frame every cfg.FPS seconds and returns how
// many were produced. Because the temporary directory is fresh, stale files
// from an earlier run can never be mistaken for new frames.
func extractThumbnails(ctx context.Context, cfg config) (int, error) {
	fmt.Println("Generating thumbnails...")
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, fmt.Errorf("ffmpeg is not available: %w", err)
	}

	filter := fmt.Sprintf(
		"fps=1/%d,scale=%d:%d:force_original_aspect_ratio=decrease,"+
			"pad=%d:%d:(ow-iw)/2:(oh-ih)/2:black",
		cfg.FPS, cfg.TileWidth, cfg.TileHeight, cfg.TileWidth, cfg.TileHeight)

	cmd := exec.CommandContext(ctx, ffmpeg,
		"-y", "-i", cfg.InputFile,
		"-vf", filter,
		"-q:v", "2",
		filepath.Join(cfg.ThumbnailDir, thumbnailNamePattern))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("generating thumbnails: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(cfg.ThumbnailDir, "thumb*.jpg"))
	if err != nil {
		return 0, fmt.Errorf("reading thumbnails: %w", err)
	}
	if len(files) == 0 {
		return 0, ErrNoFrames
	}
	fmt.Printf("Generated %d thumbnails.\n", len(files))
	return len(files), nil
}

// buildPages splits the extracted thumbnails into pages of at most
// Columns*Rows tiles. Cue times use the original video timeline (tile i starts
// at i*FPS seconds), so time keeps increasing across page boundaries. Unused
// cells on the final page get neither a tile nor a cue.
func buildPages(cfg config, thumbnailCount int, duration time.Duration) []page {
	capacity := cfg.Columns * cfg.Rows
	if capacity <= 0 || thumbnailCount <= 0 {
		return nil
	}
	interval := time.Duration(cfg.FPS) * time.Second
	pageCount := (thumbnailCount + capacity - 1) / capacity

	pages := make([]page, 0, pageCount)
	for p := 0; p < pageCount; p++ {
		first := p * capacity
		count := min(capacity, thumbnailCount-first)
		grid := pageLayout(cfg, count)
		pg := page{
			FirstTile: first,
			TileCount: count,
			Grid:      grid,
			Cues:      make([]cue, 0, count),
			ImageName: imageName(p),
		}
		for i := 0; i < count; i++ {
			tile := first + i
			start := time.Duration(tile) * interval
			end := start + interval
			// Never advertise time past the end of the source video, but keep
			// every cue non-empty even if ffprobe reported a short duration.
			if duration > start && end > duration {
				end = duration
			}
			pg.Cues = append(pg.Cues, cue{
				Start: start,
				End:   end,
				X:     (i % grid.Columns) * cfg.TileWidth,
				Y:     (i / grid.Columns) * cfg.TileHeight,
			})
		}
		pages = append(pages, pg)
	}
	return pages
}

// pageLayout picks the grid for count tiles. A full page uses the configured
// maximum. A partial page (typically the last one) uses the smallest grid that
// fits the tiles, preferring wider layouts on ties, so no row of blank cells is
// appended after the final thumbnail.
func pageLayout(cfg config, count int) layout {
	if count >= cfg.Columns*cfg.Rows {
		return layout{Columns: cfg.Columns, Rows: cfg.Rows}
	}
	best := layout{Columns: 1, Rows: count}
	bestArea := count
	for columns := 1; columns <= cfg.Columns; columns++ {
		rows := (count + columns - 1) / columns
		if rows > cfg.Rows {
			continue
		}
		area := columns * rows
		if area < bestArea || (area == bestArea && columns > best.Columns) {
			best = layout{Columns: columns, Rows: rows}
			bestArea = area
		}
	}
	return best
}

// assemblePage renders exactly TileCount tiles into a temporary file, using the
// page's concrete grid so a partial final page has no blank cells.
func assemblePage(ctx context.Context, cfg config, p page) (string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", fmt.Errorf("ffmpeg is not available: %w", err)
	}

	fmt.Printf("Creating %s (%dx%d tiles)...\n", p.ImageName, p.Grid.Columns, p.Grid.Rows)

	tmp, err := os.CreateTemp(cfg.OutputDir, ".storyboard-page-*.jpg")
	if err != nil {
		return "", fmt.Errorf("creating temporary storyboard file: %w", err)
	}
	tmpPath := tmp.Name()
	tmp.Close()
	os.Chmod(tmpPath, 0644)

	cmd := exec.CommandContext(ctx, ffmpeg,
		"-y",
		"-start_number", strconv.Itoa(p.FirstTile+1),
		"-i", filepath.Join(cfg.ThumbnailDir, thumbnailNamePattern),
		"-vf", "tile="+p.Grid.String()+fmt.Sprintf(":nb_frames=%d", p.TileCount),
		"-frames:v", "1",
		"-update", "1",
		tmpPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		removeFiles(tmpPath)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("creating storyboard image %s: %w", p.ImageName, err)
	}
	return tmpPath, nil
}

// renderVTT emits one cue per thumbnail, in order. Every cue names the page
// that contains its tile, so cross-page cues point at the right image and
// rectangle.
func renderVTT(cfg config, pages []page) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, p := range pages {
		imageURL := cfg.pageURL(p)
		for _, c := range p.Cues {
			fmt.Fprintf(&b, "%s --> %s\n", formatDuration(c.Start), formatDuration(c.End))
			fmt.Fprintf(&b, "%s#xywh=%d,%d,%d,%d\n\n",
				imageURL, c.X, c.Y, cfg.TileWidth, cfg.TileHeight)
		}
	}
	return b.String()
}

// writeVTT writes the rendered cues directly. The command flow uses
// writeVTTTemp instead so a failed run never leaves a partial VTT behind.
func writeVTT(cfg config, pages []page, path string) error {
	if err := os.WriteFile(path, []byte(renderVTT(cfg, pages)), 0644); err != nil {
		return fmt.Errorf("writing VTT file: %w", err)
	}
	return nil
}

func writeVTTTemp(ctx context.Context, cfg config, pages []page) (string, error) {
	content := renderVTT(cfg, pages)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(cfg.OutputDir, ".storyboard-vtt-*.vtt")
	if err != nil {
		return "", fmt.Errorf("creating temporary VTT file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := io.WriteString(tmp, content); err != nil {
		tmp.Close()
		removeFiles(tmpPath)
		return "", fmt.Errorf("writing VTT file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		removeFiles(tmpPath)
		return "", fmt.Errorf("writing VTT file: %w", err)
	}
	os.Chmod(tmpPath, 0644)
	return tmpPath, nil
}

// commitOutputs renames every fully built temporary file to its final name.
// All content is complete at this point, so renaming is a quick final step.
func commitOutputs(cfg config, pages []page, temps []string) error {
	finalPaths := make([]string, 0, len(pages)+1)
	for _, p := range pages {
		finalPaths = append(finalPaths, filepath.Join(cfg.OutputDir, p.ImageName))
	}
	finalPaths = append(finalPaths, filepath.Join(cfg.OutputDir, "storyboard.vtt"))

	committed := make([]string, 0, len(finalPaths))
	for i, tmp := range temps {
		if err := os.Rename(tmp, finalPaths[i]); err != nil {
			removeFiles(committed...)
			return fmt.Errorf("writing %s: %w", filepath.Base(finalPaths[i]), err)
		}
		committed = append(committed, finalPaths[i])
	}
	fmt.Println("Storyboard VTT file generated successfully.")
	return nil
}

// removeStalePages deletes storyboard-2.jpg, storyboard-3.jpg, ... left over
// from an earlier -force run that produced more pages. Without this, the old
// page count would survive and the VTT and images could describe different
// files. Only exact page names are touched; the first page is never removed.
func removeStalePages(cfg config, pageCount int) {
	for number := pageCount; number < pageCount+1024; number++ {
		path := filepath.Join(cfg.OutputDir, imageName(number))
		if _, err := os.Stat(path); err != nil {
			return
		}
		removeFiles(path)
	}
}

// checkBaseOutputConflicts rejects overwriting the one image and VTT that every
// run writes, before any thumbnail extraction work is done.
func checkBaseOutputConflicts(cfg config) error {
	if cfg.Force {
		return nil
	}
	return checkMissing(filepath.Join(cfg.OutputDir, imageName(0)),
		filepath.Join(cfg.OutputDir, "storyboard.vtt"))
}

func checkOutputConflicts(cfg config, pages []page) error {
	if cfg.Force {
		return nil
	}
	targets := make([]string, 0, len(pages)+1)
	for _, p := range pages {
		targets = append(targets, filepath.Join(cfg.OutputDir, p.ImageName))
	}
	targets = append(targets, filepath.Join(cfg.OutputDir, "storyboard.vtt"))
	return checkMissing(targets...)
}

// checkMissing returns ErrOutputExists for the first path that is present.
func checkMissing(targets ...string) error {
	for _, target := range targets {
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("%w: %s (use -force to replace it)", ErrOutputExists, target)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("checking %s: %w", target, err)
		}
	}
	return nil
}

// imageName keeps the original single-page filename for short videos and adds
// a numeric suffix only when the video needs more than one page.
func imageName(number int) string {
	if number == 0 {
		return "storyboard.jpg"
	}
	return fmt.Sprintf("storyboard-%d.jpg", number+1)
}

func (cfg config) pageURL(p page) string {
	return cfg.BaseURL + "/" + p.ImageName
}

func removeFiles(paths ...string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int64(d / time.Second)
	h := seconds / 3600
	m := (seconds % 3600) / 60
	s := seconds % 60
	ms := d.Milliseconds() % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

// ---------------------------------------------------------------------------
// Original API
//
// These entry points keep the behaviour and signatures of the first version,
// so existing callers and tests continue to work. The paged command line flow
// above is the supported path; these wrappers remain for compatibility.
// ---------------------------------------------------------------------------

func showUsage() {
	fmt.Println("Usage: <executable> <fps> <input_file> <output_dir>")
	fmt.Println("./storyboard 1 example.mp4 ./output")
	os.Exit(1)
}

func parseArguments() (int, string, string) {
	fps, err := strconv.Atoi(os.Args[1])
	if err != nil || fps <= 0 {
		fmt.Println("Error: Invalid FPS value. Please provide a positive integer.")
		os.Exit(1)
	}
	return fps, os.Args[2], os.Args[3]
}

func ensureDirectoryExists(path string) {
	if err := os.MkdirAll(path, os.ModePerm); err != nil {
		fmt.Printf("Failed to create directory %s: %v\n", path, err)
		os.Exit(1)
	}
}

func generateThumbnails(fps int, inputFile, thumbnailsDir string) {
	fmt.Println("Generating thumbnails...")
	cmd := exec.Command("ffmpeg", "-i", inputFile, "-vf", fmt.Sprintf("fps=1/%d,scale=384:160", fps),
		filepath.Join(thumbnailsDir, "thumb%05d.jpg"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("Error generating thumbnails: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Thumbnails generated successfully.")
}

func generateStoryboard(thumbnailsDir, storyboardImage string) {
	fmt.Println("Creating storyboard.jpg...")
	thumbnails, err := filepath.Glob(filepath.Join(thumbnailsDir, "thumb*.jpg"))
	if err != nil {
		fmt.Printf("Error reading thumbnails: %v\n", err)
		os.Exit(1)
	}
	if len(thumbnails) == 0 {
		fmt.Println("No thumbnails found. Exiting.")
		os.Exit(1)
	}

	columns := 10
	rows := (len(thumbnails) + columns - 1) / columns
	tileFilter := fmt.Sprintf("tile=%dx%d", columns, rows)

	cmd := exec.Command("ffmpeg", "-pattern_type", "glob", "-i", filepath.Join(thumbnailsDir, "*.jpg"), "-filter_complex", tileFilter, storyboardImage)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("Error creating storyboard image: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Storyboard image created successfully.")
}

func generateVTT(fps int, thumbnailsDir, vttFile string) {
	fmt.Println("Generating storyboard.vtt...")
	thumbnails, err := filepath.Glob(filepath.Join(thumbnailsDir, "thumb*.jpg"))
	if err != nil {
		fmt.Printf("Error reading thumbnails: %v\n", err)
		os.Exit(1)
	}

	durationPerThumb := 1.0 * float64(fps)
	vttContent := "WEBVTT\n\n"
	for i := range thumbnails {
		start := time.Duration(float64(i) * durationPerThumb * float64(time.Second))
		end := start + time.Duration(durationPerThumb*float64(time.Second))

		x := (i % 10) * 384
		y := (i / 10) * 160
		vttContent += fmt.Sprintf("%s --> %s\n", formatDuration(start), formatDuration(end))
		vttContent += fmt.Sprintf("https://insertlinkhere.com/storyboard.jpg#xywh=%d,%d,384,160\n\n", x, y)
	}

	if err := os.WriteFile(vttFile, []byte(vttContent), 0644); err != nil {
		fmt.Printf("Error writing VTT file: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Storyboard VTT file generated successfully.")
}

func cleanup(thumbnailsDir string) {
	fmt.Println("Cleaning up temporary files...")
	if err := os.RemoveAll(thumbnailsDir); err != nil {
		fmt.Printf("Error cleaning up thumbnails: %v\n", err)
		os.Exit(1)
	}
}
