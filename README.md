Two modules in one repository:

| Module | Path | What it is |
|---|---|---|
| `github.com/memmaker/gollum` | `llm/` | The library: providers, streaming, sessions, tool plumbing. Standard library only. |
| `github.com/memmaker/crash` | `crash/` | The harness: TUI, config files, the tools that talk to a user. |

The library ships no tools, no terminal code and no config format. It takes an
`llm.Config` and is done; deciding where that config comes from is the
harness's job (`crash/config.go`, backed by `github.com/memmaker/rec-files`). Anything that
touches a terminal, a filesystem or a user lives in `crash`.

## Running the CLI

```bash
git clone https://github.com/memmaker/gollum.git
cd gollum/crash
go build -o crash
```

Interactive is the default:

```bash
./crash
```

Batch mode with `-b` reads the prompt from stdin, prints the answer and exits:

```bash
echo "explain go channels" | ./crash -b
```

The interactive mode is a four-pane TUI: a scrollable output pane, a scrollable
status sidebar on the right (dropped below 90 columns), the input box, and a
help line that lists the keys of whichever pane has focus. Both panes carry
their own scrollbar, and a gradient bar across the top animates for as long as
a request chain is running. The mouse wheel scrolls the focused pane, and dragging selects whole lines
within a single pane and copies them to the system clipboard on release. The
sidebar reports the model, token usage, turn count and working directory.

| Key | Does |
|---|---|
| `enter` | send |
| `alt+enter` | newline in the input box |
| `tab` | next pane (input → output → status) |
| `↑`/`↓`/`pgup`/`pgdn`, `g`/`G` | scroll the focused pane |
| `↑`/`↓` in the input box | previous/next prompt from the history |
| `ctrl+c` | quit |
| `ctrl+p` | main menu |
| `ctrl+s` | session on/off |
| drag in a pane | select lines, copied on release |
| `esc` | close an overlay |

`/model` and any tool that asks a question open a filterable list overlay
composited over the panes — nothing drops back to the plain terminal.

The main menu holds the settings that outlive a run: send tools, full output
width, session, TTS for answers and TTS language, plus the last request's
context and the per-request token costs. Toggles stay on screen when flipped,
so the menu doubles as the display.

Prompts you send are appended to `~/.config/gollum/history` and come back with
`↑` in the input box. A prompt that spans lines is stored with its newlines
escaped, so one prompt stays one line in the file.

Type `/model` to switch provider (the choice is saved as the default),
`/debug` to show tool arguments and results in full instead of one-line
previews, or `/exit` to quit. Tool calls are drawn in the transcript where they
happened, with each result under its call. Flags: `-model` (override the configured model), `-raw` (skip
markdown rendering), `-timeout` (give up when a backend sends nothing for this
long), `-b` (batch mode).

### Session

On — the default — each prompt is sent with the conversation so far, and the
context window grows with it. Off, every prompt is asked cold: the model sees
the system prompt and that prompt alone. The transcript keeps being recorded
either way, so turning the session back on resumes with everything that was
said while it was off. While it is off the logo carries `OBLIVIUM` next to its
tagline, because a conversation that is not being remembered should not look
like one that is.

### Speaking answers

With `TTS for answers` on, each finished block of an answer is read aloud by a
local engine, started on the first block that needs it and kept loaded until
the app exits. Code blocks, tables and markdown syntax are dropped — the
spoken text comes from the same parse the renderer draws from, so the two
cannot disagree. Sending a new prompt cuts off whatever is still being read.

The engine is any command that speaks the `--serve` protocol: one line of text
on stdin, one `TTS_TIME <seconds> <path.wav>` line back. Engines are per
language and configured in the settings record; `de` and `en` ship pointing at
kokoro, and any other key works the same way.

### Config

Config lives in `./llm_model.rec`, else `~/.config/gollum/config.rec` — one
record per provider, fields `provider`, `model`, `base_url`, `api_key_file`.
API keys are read from the provider's environment variables first.
`~/.config/gollum/system.md`, if present, is the system prompt.

The menu's own settings share that file in a record tagged `settings: ui`, so
the provider records are never touched by a save:

```
settings: ui
send_tools: on
full_width: off
session: on
tts_answers: off
tts_lang: de
tts_engine_de: tts-de-male-kokoro
tts_engine_en: tts-en-male-kokoro
```

## Using the library

```bash
go get github.com/memmaker/gollum
```

The caller supplies the config — there is no file format to adopt:

```go
import "github.com/memmaker/gollum/llm"

cfg := llm.Config{Provider: "openai", APIKey: key, Model: "gpt-5-mini"}
client, err := llm.New(cfg)
sess := llm.NewSession(client, systemPrompt)
sess.Model = cfg.Model

ch, err := sess.StreamWithTools(ctx, "hello")
for msg := range ch {
    fmt.Print(msg.Content)
}
if err := sess.Err(); err != nil { /* stream failed mid-flight */ }
```

`sess.Stateless` leaves the conversation out of the request — each turn is sent
with the system prompt and the new user message alone. The transcript is still
recorded, so clearing it again resumes with everything said in between.

Register your own tools on the session — a session starts with none:

```go
sess.AddTool(llm.Tool{
    Type: "function",
    Function: llm.ToolFunction{
        Name:        "roll",
        Description: "Roll a six-sided die.",
        Parameters:  map[string]any{ /* JSON schema */ },
    },
}, func(ctx context.Context, call llm.ToolCall) (string, error) {
    return "4", nil
})
```

See `crash/tools.go` for a working example.

## Developing both modules

`crash/go.mod` carries `replace github.com/memmaker/gollum => ../`, so local
library changes are picked up without a release. Build and test each module
from its own directory:

```bash
go test ./... && (cd crash && go test ./...)
```
