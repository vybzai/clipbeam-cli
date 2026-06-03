# clipbeam — CLI tour, rendered with vhs (https://github.com/charmbracelet/vhs):
#   PATH=/opt/homebrew/bin:$PATH vhs docs/demo.tape   (needs a `clipbeam` on PATH)
# This tours the agent-first surface; the full beam-to-remote flow needs a live
# box (drag an image -> `clipbeam send` -> the box's agent reads `clipbeam last`).
Output docs/demo.gif

Set Shell "bash"
Set FontSize 18
Set Width 1180
Set Height 720
Set Padding 22
Set Theme "Dracula"
Set TypingSpeed 45ms

Hide
Type "export PATH=/tmp/cbdemo:$PATH; clear" Enter
Show

Sleep 600ms
Type "clipbeam" Sleep 400ms Enter
Sleep 3.5s

Type "clipbeam version --json" Sleep 400ms Enter
Sleep 2.8s

Type "clipbeam doctor" Sleep 400ms Enter
Sleep 2.8s

# The headline workflow (typed, not run — needs a paired box):
Type "clipbeam send screenshot.png my-box   # prints the remote path your agent reads"
Sleep 3.5s
