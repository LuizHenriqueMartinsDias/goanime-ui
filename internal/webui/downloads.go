package webui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alvarorichard/Goanime/internal/api/providers"
	"github.com/alvarorichard/Goanime/internal/util"
)

// Job is one episode download.
type Job struct {
	ID       int     `json:"id"`
	AnimeID  string  `json:"animeId"`
	Title    string  `json:"title"`
	Episode  string  `json:"episode"`
	State    string  `json:"state"` // queued | resolving | downloading | error
	Progress float64 `json:"progress"`
	Speed    float64 `json:"speed,omitempty"` // bytes/s
	Error    string  `json:"error,omitempty"`

	quality string
	cancel  context.CancelFunc
}

// DownloadedFile is a finished episode on disk.
type DownloadedFile struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Downloads runs a small queue of yt-dlp downloads into GoAnime's own
// download folder, named the way the CLI names them.
type Downloads struct {
	catalog *Catalog
	dir     string

	mu     sync.Mutex
	jobs   []*Job
	nextID int
	wake   chan struct{}
}

const downloadWorkers = 2

func NewDownloads(c *Catalog, dir string) *Downloads {
	d := &Downloads{catalog: c, dir: dir, wake: make(chan struct{}, downloadWorkers)}
	for range downloadWorkers {
		go d.worker()
	}
	return d
}

func (d *Downloads) Dir() string { return d.dir }

// Enqueue adds one job per episode label.
func (d *Downloads) Enqueue(id string, labels []string, quality string) error {
	a, ok := d.catalog.Anime(id)
	if !ok {
		return errNotFound
	}
	title, _ := splitTitle(a.Name)
	d.mu.Lock()
	for _, l := range labels {
		if d.activeLocked(id, l) {
			continue
		}
		d.nextID++
		d.jobs = append(d.jobs, &Job{ID: d.nextID, AnimeID: id, Title: title, Episode: l, State: "queued", quality: quality})
	}
	d.mu.Unlock()
	for range downloadWorkers {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (d *Downloads) activeLocked(id, label string) bool {
	for _, j := range d.jobs {
		if j.AnimeID == id && j.Episode == label && j.State != "error" {
			return true
		}
	}
	return false
}

// Jobs returns a snapshot of the queue.
func (d *Downloads) Jobs() []Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Job, len(d.jobs))
	for i, j := range d.jobs {
		out[i] = *j
		out[i].cancel = nil
	}
	return out
}

// Cancel stops a running job or drops a queued/failed one.
func (d *Downloads) Cancel(jobID int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, j := range d.jobs {
		if j.ID == jobID {
			if j.cancel != nil {
				j.cancel()
			}
			d.jobs = append(d.jobs[:i], d.jobs[i+1:]...)
			return
		}
	}
}

func (d *Downloads) worker() {
	for {
		j := d.claim()
		if j == nil {
			<-d.wake
			continue
		}
		err := d.run(j)
		d.mu.Lock()
		idx := -1
		for i, x := range d.jobs {
			if x == j {
				idx = i
			}
		}
		if idx >= 0 {
			if err != nil && !errors.Is(err, context.Canceled) {
				j.State, j.Error, j.cancel = "error", err.Error(), nil
			} else {
				d.jobs = append(d.jobs[:idx], d.jobs[idx+1:]...)
			}
		}
		d.mu.Unlock()
	}
}

func (d *Downloads) claim() *Job {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, j := range d.jobs {
		if j.State == "queued" {
			j.State = "resolving"
			return j
		}
	}
	return nil
}

func (d *Downloads) update(j *Job, fn func(*Job)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	fn(j)
}

func (d *Downloads) run(j *Job) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.update(j, func(j *Job) { j.cancel = cancel })

	a, ep, err := d.catalog.Episode(ctx, j.AnimeID, j.Episode)
	if err != nil {
		return err
	}

	resolveMu.Lock()
	streamURL, err := providers.FetchStreamURL(ctx, ep, a, j.quality)
	referer := util.GetGlobalReferer()
	resolveMu.Unlock()
	if err != nil {
		return fmt.Errorf("não foi possível obter o vídeo: %w", err)
	}

	name := util.SanitizeForFilename(j.Title)
	dest := filepath.Join(d.dir, name, fmt.Sprintf("%s - S01E%s.mp4", name, j.Episode))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	args := []string{
		"--newline", "--no-playlist", "--no-warnings",
		"--concurrent-fragments", "4",
		"--progress-template", "download:GA|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|%(progress.total_bytes_estimate)s|%(progress.fragment_index)s|%(progress.fragment_count)s|%(progress.speed)s",
		"-o", dest,
	}
	if referer != "" {
		args = append(args, "--add-headers", "Referer:"+referer)
	}
	args = append(args, "--", streamURL)

	cmd := exec.CommandContext(ctx, "yt-dlp", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("yt-dlp não iniciou: %w", err)
	}
	d.update(j, func(j *Job) { j.State = "downloading" })

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		if pct, speed, ok := parseProgress(sc.Text()); ok {
			d.update(j, func(j *Job) { j.Progress, j.Speed = pct, speed })
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			removePartials(dest)
			return ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if i := strings.LastIndex(msg, "\n"); i >= 0 {
			msg = msg[i+1:]
		}
		return fmt.Errorf("download falhou: %s", msg)
	}
	return nil
}

// parseProgress reads one line of the --progress-template set in run.
func parseProgress(line string) (pct, speed float64, ok bool) {
	rest, found := strings.CutPrefix(line, "GA|")
	if !found {
		return 0, 0, false
	}
	f := strings.Split(rest, "|")
	if len(f) != 6 {
		return 0, 0, false
	}
	num := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return v
	}
	done, total, estimate, fragIdx, fragCount := num(f[0]), num(f[1]), num(f[2]), num(f[3]), num(f[4])
	switch {
	case total > 0:
		pct = done / total
	case fragCount > 0:
		pct = fragIdx / fragCount
	case estimate > 0:
		pct = done / estimate
	}
	return min(pct, 1), num(f[5]), true
}

func removePartials(dest string) {
	matches, _ := filepath.Glob(dest + "*.part*")
	for _, m := range matches {
		_ = os.Remove(m)
	}
	_ = os.Remove(dest + ".part")
}

var videoExts = map[string]bool{".mp4": true, ".mkv": true, ".webm": true, ".ts": true}

// Files lists finished videos in the download folder, newest first.
func (d *Downloads) Files() []DownloadedFile {
	var out []DownloadedFile
	_ = filepath.WalkDir(d.dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !videoExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		out = append(out, DownloadedFile{Name: e.Name(), Path: path, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// Contains reports whether path is a file inside the download folder, so the
// UI can only ask mpv to open what GoAnime downloaded.
func (d *Downloads) Contains(path string) bool {
	rel, err := filepath.Rel(d.dir, filepath.Clean(path))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
