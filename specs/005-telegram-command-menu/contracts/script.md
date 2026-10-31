# Contract: Custom Script Command List

`TELEGRAM_UNKNOWN_SCRIPT`, run with **no arguments** once at start, prints its commands on
stdout, one per line, in BotFather's format:

```text
name - description
```

| Part | Rule |
| --- | --- |
| `name` | 1–32 of `a-z`, `0-9`, `_`; one leading `/` is allowed and dropped |
| separator | the first ` - ` (space, hyphen, space) on the line |
| `description` | 1–256 characters after trimming; shown in the menu. Put usage here when the command takes parameters, e.g. `Wake a PC: /wake <host>` |

- Blank lines are ignored.
- Any other line is skipped with a warning naming it; the remaining lines still count.
- A name equal to a built-in command, or repeated, is skipped with a warning.
- The old single-line `a|b|c` output is **not** accepted (it has no ` - `, so it is skipped).
- Execution is unchanged: a listed command `/name args…` runs the script with the whole message
  as its argument and `TELEGRAM_THREAD_ID` set for topic messages.
- A command that takes parameters should reply with its usage when sent bare: the menu sends a
  tapped command without arguments.

Example:

```sh
#!/bin/sh
if [ $# -eq 0 ]; then
  cat <<'EOF'
wake - Wake a PC: /wake <host>
backup - Run the backup now
EOF
  exit 0
fi
# … handle "$1" …
```
