#!/bin/sh
set -e
# ~/bin may not exist yet on a fresh machine; go build won't create it.
mkdir -p "$HOME/bin"
# -C instead of cd: crash reads ./llm_model.rec from the working directory,
# so the shell must stay where the user ran this from.
go build -C "$(dirname "$0")/crash" -o "$HOME/bin/crash" .
echo "installed $HOME/bin/crash"
case ":$PATH:" in
  *":$HOME/bin:"*) ;;
  *) echo "warning: $HOME/bin is not on PATH" >&2 ;;
esac
