# Video Storyboard or Seek Thumbnails Generator

This Go application generates video storyboards by extracting thumbnails from a given video file. It uses FFmpeg to create the thumbnails and assemble them into a storyboard image. The program also generates a VTT file for use with the storyboard, allowing each thumbnail to be linked with the corresponding time in the video.

## Features

- Extracts thumbnails from a video at a specified frame rate (FPS).
- Generates storyboard images by arranging thumbnails in a grid.
- Splits long videos across multiple storyboard pages so a single image never
  grows beyond the configured `columns x rows` capacity.
- Creates a WebVTT file that links each thumbnail to its timestamp and to the
  correct page/rectangle in the video.
- Cleans up temporary thumbnail files after the process is completed.

## Prerequisites

- [Go](https://golang.org/dl/) version 1.18 or higher.
- [FFmpeg](https://ffmpeg.org/download.html), including `ffprobe`, installed
  and available in your system's `PATH`.

## Installation

1. Clone the repository:

   ```bash
   git clone https://github.com/yourusername/storyboard-generator.git
   cd storyboard-generator
   ```

2. Ensure Go and FFmpeg are properly installed on your system.

3. Build the Go executable:

   ```bash
   go build -o storyboard
   ```

## Usage

To use the program, run it with the following arguments:

```bash
./storyboard [options] <fps> <input_file> <output_dir>
```

### Arguments:

- `<fps>`: The frame rate (FPS) for extracting thumbnails from the video (e.g., 1 for one thumbnail per second).
- `<input_file>`: The path to the video file (e.g., `example.mp4`).
- `<output_dir>`: The directory where the storyboard image and VTT file will be saved (e.g., `./output`).

### Options:

- `-columns N`: Maximum thumbnails per row (default 10).
- `-rows N`: Maximum thumbnails per column (default 10).
- `-width N` / `-height N`: Tile size in pixels (default 384x160).
- `-base-url URL`: Base URL used in the VTT image links.
- `-force`: Replace existing output files. Without it, the run fails rather than
  overwrite anything.
- `-duration SECONDS`: Supply the video duration and skip `ffprobe`.
- `-require-duration`: Fail when the video duration cannot be determined.

### Example:

```bash
./storyboard 1 example.mp4 ./output
```

This will:

- Extract thumbnails from `example.mp4` at a rate of 1 frame per second.
- Save the thumbnails and generated files in the `./output` directory.
- Create a storyboard image named `storyboard.jpg`.
- Create a VTT file named `storyboard.vtt`.

A two hour recording at 1 FPS yields 7200 thumbnails. With the default 10x10
capacity that becomes 72 pages:

```bash
./storyboard -columns 10 -rows 10 1 lecture.mp4 ./output
# storyboard.jpg, storyboard-2.jpg, ..., storyboard-72.jpg, storyboard.vtt
```

The first page keeps the original `storyboard.jpg` name so short videos still
produce exactly one image. Pages after it are numbered `storyboard-2.jpg`,
`storyboard-3.jpg`, and so on.

## Output Files

- **storyboard.jpg**, **storyboard-2.jpg**, ...: Page images, each holding at
  most `columns * rows` thumbnails. A partial last page shrinks its grid instead
  of drawing blank cells.
- **storyboard.vtt**: A WebVTT file that links each thumbnail to its timestamp
  and to the page image plus `#xywh` rectangle that contains it.

### VTT behaviour

- Cue time uses the original video timeline: thumbnail `i` starts at
  `i * fps` seconds and continues across page boundaries without resetting.
- Each cue refers to the page that actually contains its tile, e.g.
  `storyboard-3.jpg#xywh=768,320,384,160`.
- The last page emits only real thumbnails; no cues are generated for unused
  grid cells.
- If the video duration is known, the final cue is clamped so it does not run
  past the end of the video.

### Failure behaviour

- **Unknown duration**: cue timing needs only the sampling interval, so the run
  continues with a warning; `-require-duration` turns this into an error.
- **Extraction failure or no frames**: the run exits non-zero and leaves no
  output behind.
- **Existing output**: the run stops before extracting anything unless `-force`
  is passed.
- **Canceled (Ctrl-C / SIGTERM)**: the FFmpeg child process is stopped, all
  temporary files and uncommitted pages are removed, and the exit status is
  130.

Pages and the VTT are written to temporary files and only renamed into place
once every page has succeeded, so a failed run never leaves a half-written
storyboard.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
