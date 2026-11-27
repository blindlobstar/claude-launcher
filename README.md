# claude-launcher

A small web page that creates folders for Claude to work in and keeps one
`claude remote-control` server running per folder, so each folder can be
picked in claude.ai/code and the Claude app, with its own memory.

It runs as whoever starts it, and the folder it is started in holds Claude's
folders as subfolders. Build it, then run it there:

    go build
    cd ~/projects && /path/to/claude-launcher

| Variable | Default | |
|---|---|---|
| `LAUNCHER_ADDR` | `127.0.0.1:8787` | where the page listens |
| `LAUNCHER_HOSTS` | none | names the page is opened by, such as `claude.local`; needed to listen beyond loopback without a password |
| `LAUNCHER_PASSWORD` | none | asks for this password (any user name) |
| `LAUNCHER_PERMISSION_MODE` | `default` | passed to `claude remote-control` |

Everything else in its environment, such as `GH_TOKEN`, reaches Claude.

The page lists the folders with a toggle each, the programs Claude needs and
the Claude and GitHub accounts. A folder that is on is offered in the app;
chats are started there, the launcher opens none. Deleting a folder also
removes Claude's memory and chats for it from the machine (`claude purge`).

It installs and updates Claude Code and `gh` for the current user; `git` has
to be there already. When folders can't be switched on, an alert at the top
holds the fix: installing Claude Code, signing in (link out, code in) or the
one-time "Enable Remote Control?" answer.

As a container, with the folders and Claude's home folder (sign-in,
installed programs) kept in volumes:

    docker build -t claude-launcher .
    docker run -p 8787:8787 -e LAUNCHER_HOSTS=localhost \
        -v projects:/projects -v claude-home:/home/claude claude-launcher

Stopping the launcher stops its servers; after a restart, switch the folders
on again from the page.

Tests: `go test ./...`
