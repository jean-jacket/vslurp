# vslurp

Download a time range of a large remote MP4 without downloading the whole file.

```
vslurp [flags] START-END URL OUTPUT

vslurp 01:00:00-02:00:00 https://example.com/vod.mp4 cut.mp4   # 1h to 2h
vslurp 01:00:00-         https://example.com/vod.mp4 cut.mp4   # 1h to the end
vslurp -01:00:00         https://example.com/vod.mp4 cut.mp4   # start to 1h
```

It reads the MP4's index (`moov`), works out which bytes the range needs, downloads
only those with parallel HTTP range requests, then cuts locally with
`ffmpeg -ss START -to END -i … -c copy -avoid_negative_ts make_zero`.

A stream copy can only start on a keyframe, so the start is moved back to the
keyframe at or before START (vslurp prints how far). This avoids the hidden lead-in,
negative timestamps and edit lists ffmpeg otherwise produces, which many players and
editors mishandle. In the files tested, keyframes were about 4 s apart, so a clip can
start up to that much earlier than asked. The end time is not changed.

If a run is interrupted, run the same command again and it resumes. Different cuts
of the same URL reuse whatever was already downloaded.

## Watching while it downloads (`-stream`)

With `-stream`, vslurp writes OUTPUT itself instead of using ffmpeg: a new index for
just the cut first, then the media data in order as it downloads. Play it while it
grows with:

```
mpv appending://cut.mp4
```

The video packets are identical to ffmpeg's cut. If the download stalls for more than
about 2 seconds while mpv has caught up, mpv treats it as the end of the file and
stops; reopen it with `--start=<position>` to carry on. Writing to a named pipe is not
supported yet.

## Flags

| Flag | Default | |
|---|---|---|
| `-j` | 4 | parallel connections |
| `-part-size` | 8 | download part size, MB |
| `-pad` | 2 | extra seconds fetched on each side of the range |
| `-user-agent` | `vslurp/0.1 (ffmpeg-based MP4 cutter)` | User-Agent header |
| `-stream` | off | write OUTPUT in playback order while downloading (see above) |
| `-cache-dir` | `$TMPDIR/vslurp` | where partial downloads are kept |

## Requirements

- `ffmpeg` on `PATH`
- The server must support HTTP range requests
- Regular (non-fragmented) MP4 files
- macOS or Linux

## Cache

Each URL gets a folder under the cache dir holding a sparse copy of the remote file
(only downloaded parts use disk space), a log of downloaded byte ranges, and the
file's ETag. If the remote file changes, the cache for it is discarded. The OS clears
the temp dir on restart.

## Build

```
go build -o vslurp ./bin
```
