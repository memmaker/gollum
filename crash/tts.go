package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	markterm "github.com/memmaker/gollum/mark-term"
)

// tts speaks assistant prose through a local engine kept hot in --serve mode:
// one line of text in, one `TTS_TIME <secs> <path.wav>` line out, then afplay.
// The cold model load (~6.6s) happens once, on the worker goroutine, so the
// first toggle never blocks the event loop.
type tts struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Scanner
	queue chan string   // blocks waiting to be synthesised
	wavs  chan wavJob   // synthesised and waiting their turn to play
	done  chan struct{} // closed once both workers have stopped

	mu   sync.Mutex
	play *exec.Cmd // the afplay running right now, so Stop can cut it off
	gen  int       // bumped by Stop, so a wav synthesised before it is not played
	dead bool
}

// wavJob is a finished render waiting for the speaker, tagged with the
// generation it was requested in so a Stop can discard it.
type wavJob struct {
	path string
	gen  int
}

// lookahead bounds how far synthesis may run ahead of playback. It only has to
// cover the gap between one render finishing and the next starting, so the
// engine never idles; renders past that are temp files nobody is waiting for.
const lookahead = 4

// startTTS spawns the engine. A missing binary is reported here, immediately;
// anything that goes wrong later surfaces through onFail.
func startTTS(engine string, onFail func(error)) (*tts, error) {
	if _, err := exec.LookPath(engine); err != nil {
		return nil, fmt.Errorf("engine not found: %s: %w", engine, err)
	}
	cmd := exec.Command(engine, "--serve")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &tts{
		cmd:   cmd,
		in:    in,
		out:   bufio.NewScanner(out),
		queue: make(chan string, 32),
		wavs:  make(chan wavJob, lookahead),
		done:  make(chan struct{}),
	}
	// Synthesis and playback are separate goroutines on purpose: a single loop
	// leaves the engine idle for the whole of afplay, so every block after the
	// first pays its full render time as a gap. Split, the next block is
	// already rendered — usually before the current one stops playing.
	var running sync.WaitGroup
	running.Add(2)
	go func() { defer running.Done(); t.synthesise(onFail) }()
	go func() { defer running.Done(); t.playback() }()
	go func() { running.Wait(); close(t.done) }()
	return t, nil
}

// synthesise keeps the engine fed: one line in, one wav out, handed to the
// speaker without waiting for it. It only blocks once playback is `lookahead`
// renders behind.
func (t *tts) synthesise(onFail func(error)) {
	defer close(t.wavs)
	for t.out.Scan() {
		if t.out.Text() == "SERVING" {
			break
		}
	}
	for text := range t.queue {
		t.mu.Lock()
		gen := t.gen
		t.mu.Unlock()
		if _, err := fmt.Fprintln(t.in, text); err != nil {
			t.fail(onFail)
			return
		}
		wav, err := t.reply()
		if err != nil {
			t.fail(onFail)
			return
		}
		t.wavs <- wavJob{path: wav, gen: gen}
	}
}

// playback plays finished renders in order, one at a time — there is only one
// pair of speakers.
func (t *tts) playback() {
	for job := range t.wavs {
		t.playWav(job.path, job.gen)
	}
}

// reply reads the answer to one line of text. Anything the engine logs before
// it is skipped rather than mistaken for a path — one stray line would
// otherwise pair every later reply with the wrong request.
func (t *tts) reply() (string, error) {
	for t.out.Scan() {
		// TTS_TIME 1.703s /tmp/tts.XXXX.wav — split in three so a temp
		// directory with a space in it still yields a usable path.
		parts := strings.SplitN(t.out.Text(), " ", 3)
		if len(parts) == 3 && parts[0] == "TTS_TIME" {
			return parts[2], nil
		}
	}
	return "", fmt.Errorf("engine stopped")
}

func (t *tts) fail(onFail func(error)) {
	t.mu.Lock()
	t.dead = true
	t.mu.Unlock()
	onFail(fmt.Errorf("engine stopped"))
}

// playWav plays one render, unless a Stop happened while it was being made:
// synthesis takes long enough that a question asked mid-block would otherwise
// still be answered by the previous one.
func (t *tts) playWav(path string, gen int) {
	defer os.Remove(path)
	play := exec.Command("afplay", path)
	t.mu.Lock()
	if t.dead || t.gen != gen {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	if err := play.Start(); err != nil {
		return
	}
	// Published only once it has a process: a Stop between Start failing and
	// the next assignment would otherwise kill a nil one. A Stop that lands in
	// that same window is caught by re-reading gen here.
	t.mu.Lock()
	stale := t.dead || t.gen != gen
	if !stale {
		t.play = play
	}
	t.mu.Unlock()
	if stale {
		play.Process.Kill()
		play.Wait()
		return
	}

	play.Wait() // a Stop kill lands here as an error, which is the point
	t.mu.Lock()
	t.play = nil
	t.mu.Unlock()
}

// Speak queues one block of markdown. A full queue drops the block: audio that
// far behind the screen is worse than a gap.
func (t *tts) Speak(md string) {
	text := speakable(md)
	if text == "" {
		return
	}
	select {
	case t.queue <- text:
	default:
	}
}

// Stop abandons everything pending and cuts off what is playing. The engine
// stays loaded — the next Speak is still on the hot path.
func (t *tts) Stop() {
pending:
	for {
		select {
		case <-t.queue:
		default:
			break pending
		}
	}
	// Renders that already came back are dropped too, and their files with
	// them: nothing will ever come back for one.
rendered:
	for {
		select {
		case job := <-t.wavs:
			os.Remove(job.path)
		default:
			break rendered
		}
	}
	t.mu.Lock()
	t.gen++
	if t.play != nil {
		t.play.Process.Kill()
	}
	t.mu.Unlock()
}

// Close stops playback and kills the engine, so no model is left holding the
// GPU after the TUI exits.
func (t *tts) Close() {
	t.Stop()
	t.mu.Lock()
	already := t.dead
	t.dead = true
	t.mu.Unlock()
	if !already {
		// Ends the worker's range, so nothing is left reading the pipe that
		// Wait is about to close — and no goroutine outlives the engine.
		close(t.queue)
	}
	t.in.Close()
	t.cmd.Process.Kill()
	<-t.done
	t.cmd.Wait()
}

// speak reads one block of an answer out loud, starting the engine on the
// first block that arrives. Starting here rather than at toggle time keeps the
// model load off the moment a setting is flipped — and off startup entirely
// when the toggle was persisted on.
func (m *tui) speak(md string) {
	if !m.ttsOn {
		return
	}
	if m.speech == nil {
		engine := m.ttsEngines[m.ttsLang]
		if engine == "" {
			m.ttsFailed("no engine configured for " + m.ttsLang)
			return
		}
		t, err := startTTS(engine, func(err error) {
			if prog != nil {
				prog.Send(ttsFailMsg{err})
			}
		})
		if err != nil {
			m.ttsFailed(err.Error())
			return
		}
		m.speech = t
	}
	m.speech.Speak(md)
}

// ttsUsable reports whether the engine for the current language could run, so
// the menu can refuse the toggle instead of accepting it and going quiet.
func (m *tui) ttsUsable() error {
	engine := m.ttsEngines[m.ttsLang]
	if engine == "" {
		return fmt.Errorf("no engine configured for %s", m.ttsLang)
	}
	if _, err := exec.LookPath(engine); err != nil {
		return fmt.Errorf("engine not found: %s: %w", engine, err)
	}
	return nil
}

// ttsFailed turns speech off and says why in the sidebar. The sidebar is
// redrawn here because these failures happen mid-answer, not on a keypress
// that would redraw it anyway.
func (m *tui) ttsFailed(reason string) {
	m.ttsOn = false
	m.note = "tts: " + reason
	m.refreshSidebar()
}

// setTTSLang switches voices. The old engine is dropped rather than kept warm:
// two loaded models to save one reload is a bad trade.
func (m *tui) setTTSLang(lang string) {
	if lang == m.ttsLang {
		return
	}
	m.ttsLang = lang
	if m.speech != nil {
		m.speech.Close()
		m.speech = nil
	}
	m.saveSettings()
}

// speakable turns a markdown block into what the engine should hear. The
// markdown itself is stripped by the renderer's own parser; what is left here
// is pronunciation: an identifier like `send_tools` is two words to a listener,
// never "send underscore tools".
func speakable(block string) string {
	return strings.TrimSpace(strings.ReplaceAll(markterm.PlainText(block), "_", " "))
}
