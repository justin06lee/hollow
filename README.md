<div align="center">

<img src="assets/hollow.svg" alt="hollow" width="330" />

# hollow

**Computers for bots.**<br>
*One host boots a small VM per agent and lets it see and drive the screen — self-hosted, on whatever machine has the memory.*

</div>

---

hollow is the machine room of a Grok-Bot-style setup: an always-on computer for
every agent, with a browser, a terminal and a filesystem that persist while
it works. One host builds a golden image per guest OS, boots a **desk** — a
VM with a virtual display — for every bot that asks, and answers one HTTP
API for screenshots, input, programs, files and screen recording.

It is the bottom of three pieces. **hollow** owns the VMs and the API.
[bangboo](https://github.com/justin06lee/bangboo) is the client that goes into
a desk on an agent's behalf. [phaethon](https://github.com/justin06lee/phaethon)
is the skill that tells the agent how to use bangboo well.

A Linux desk idles at well under a hundred megabytes. What costs memory is
what the bot runs on it, and a browser costs the same anywhere — so the host
defaults to 768 MB per desk and lets you turn that down or up.

## Install

On the machine that will run the VMs — a Linux box with KVM — and on any
machine you want the CLI on:

```sh
git clone https://github.com/justin06lee/hollow && cd hollow && make
```

`make` builds the guest agent into the hollow binary, installs `hollow` on
your PATH, and prints what to do next. Needs Go 1.25 or newer. The host needs
`qemu-system-x86_64` and `qemu-img` (`pacman -S qemu-base qemu-img`,
`apt install qemu-system-x86 qemu-utils`) and access to `/dev/kvm`.

The box the VMs run on is often not the machine the code is edited on:

```sh
make ship HOST=root@box      # build for linux/amd64 and install it there over ssh
```

## Run the host

```sh
hollow serve
```

```
hollow v0.1.0 — listening on 127.0.0.1:7070

  state    /var/lib/hollow
  backend  qemu with kvm
  image    linux   missing — run: hollow pull linux
  connect  hollow1-eyJ1IjoiaHR0cDovLzEyNy4wLjAuMTo3MDcwIiwidCI6Ik…
```

Then, once:

```sh
hollow pull linux
```

That downloads Alpine's stock cloud image, checks its checksum, boots it with
a provisioning script that installs the desktop and the agent hook, and keeps
the powered-off result as the golden image. A few minutes, and every desk
from then on is a copy-on-write clone of it.

To have it survive reboots and ssh sessions, on Linux:

```sh
make service            # a systemd unit, its own user in the kvm group, state in /var/lib/hollow
journalctl -u hollow -f
```

| Flag | What it does |
|---|---|
| `--addr 127.0.0.1:7070` | Address to listen on. Loopback by default; see below before changing it. |
| `--state DIR` | Where images, desks and the token live. `HOLLOW_HOME` does the same. |
| `--advertise URL` | The address to put in the printed connect code, when clients reach this host by another name. |
| `--quiet` | Print nothing but errors. |

## Connect from another machine

The API is bearer-token authenticated and listens on loopback. It is meant to
be reached through a mesh rather than exposed: on a
[makima](https://github.com/justin06lee/makima) mesh, every loopback listener
is published automatically, so the host is at `box.makima:7070` from every
other machine with nothing to configure. Tailscale and a plain LAN work the
same way with `--addr`.

A **connect code** is the address and the token in one string. Mint one for
the address clients will use, on the host:

```sh
hollow connect --url http://box.makima:7070
```

and paste it wherever hollow or bangboo runs:

```sh
export HOLLOW_CONNECT=hollow1-…
hollow status
```

`HOLLOW_URL` and `HOLLOW_TOKEN` are the two halves, for anything that would
rather not carry one string. On the host itself the CLI needs neither: it
reads the token from the state directory.

## Use a desk

```sh
hollow new                          # boots a desk, waits until it can be driven, prints its id
hollow new --name maya --mem 512 --size 1440x900
hollow ls
```

```sh
hollow shot a1b2c3                  # a PNG of the screen, with the pointer
hollow exec a1b2c3 --detach -- chromium https://example.com
hollow click a1b2c3 640 400
hollow type a1b2c3 hello there
hollow key a1b2c3 ctrl+l
hollow scroll a1b2c3 5
hollow drag a1b2c3 100 100 400 300
hollow exec a1b2c3 -- ls -la
hollow exec a1b2c3 --shell -- 'echo $DISPLAY && date'
hollow put a1b2c3 notes.txt notes.txt
hollow get a1b2c3 out.png out.png
hollow rec a1b2c3 start
hollow rec a1b2c3 stop clip.mp4
hollow rm a1b2c3
```

Programs run as the desk's user, `bot`, with the display set, passwordless
`sudo` and `doas`, and a home directory that lasts as long as the desk does.
`exec` waits up to a minute by default and returns stdout, stderr and the
exit code; `--detach` starts something and returns its pid.

## The API

Everything the CLI does is one request. `Authorization: Bearer <token>` on
all of it.

| | |
|---|---|
| `GET /v1/status` | Version, backend, images, desk count. |
| `GET /v1/images` · `GET /v1/images/{os}` | Golden images and their state. |
| `POST /v1/images/{os}/pull` | Build one. Returns at once; poll. |
| `GET /v1/desks` · `POST /v1/desks` | List desks; boot one from a `DeskSpec`. |
| `GET /v1/desks/{id}` · `DELETE /v1/desks/{id}` | One desk; stop it. |
| `GET /v1/desks/{id}/screenshot` | PNG. `?format=jpeg&quality=80` for smaller. |
| `POST /v1/desks/{id}/input` | One `Input`: move, click, dblclick, down, up, scroll, type, key, drag. |
| `POST /v1/desks/{id}/exec` | Run an `Exec`; get an `ExecResult`. |
| `PUT /v1/desks/{id}/files?path=` · `GET …/files?path=` | Write and read files on the desk. |
| `POST /v1/desks/{id}/record/start` · `…/record/stop` | Record the screen; stop returns the MP4. |
| `GET /v1/desks/{id}/health` · `GET /v1/desks/{id}/logs` | The agent's view; the serial console. |

The types are in [`api/types.go`](api/types.go) and a Go client in
[`client`](client/client.go) — both importable, which is what bangboo does.

## How it works

**Images.** `hollow pull linux` fetches Alpine's cloud image and boots it once
with a script handed in over a virtual CD-ROM as cloud-init user-data. The
script installs Xvfb, a window manager, Chromium, xdotool and ffmpeg, makes
the `bot` user, installs a boot service, and powers off. Alpine because it is
the smallest thing with a package for everything on that list.

**Desks.** A desk is a QEMU/KVM VM booted from a copy-on-write overlay of the
golden image, with a second small CD-ROM holding its configuration — hollow's
port, the screen size, its name. At boot the service reads that, starts a
virtual display at the requested size, and fetches the **agent** from the
host. The agent is built into the hollow binary and handed out at every boot,
so a new hollow never needs a new image.

**The agent.** A small program inside the desk that answers the host on a
port only the host can reach: it grabs the screen straight from X and
composites the cursor on, drives input through xdotool, runs programs as the
desk's user, and records with ffmpeg. The host forwards a client's request to
it nearly verbatim, which is why the API has one vocabulary end to end.

**Isolation.** One VM per bot. Sharing a VM between bots through separate
virtual desktops was the first idea, and it does not hold: desktops share a
pointer and a focus, and a bot on one can still take it from the other. A
separate VM costs a few tens of megabytes over a separate desktop and takes
the question away entirely.

## Known limits

- **Linux guests only, on a Linux host with KVM.** The backend is behind an
  interface so that macOS guests — which can only run on Apple hardware, under
  Apple's Virtualization framework — and Windows guests can follow. Neither is
  here yet.
- **Desks do not survive the host.** A desk is a running VM and nothing else;
  restart `hollow serve` and they are gone. Persistent homes are the next
  thing to add.
- **The API is HTTP.** It is designed to sit on a mesh or loopback, where the
  transport is already encrypted and authenticated. Bind it to a public
  address and the token is the only thing between the internet and a shell.
- **A guest can reach the host's loopback.** QEMU's user networking maps the
  host to `10.0.2.2` inside the guest, which is how the agent is fetched. The
  API needs the token, which the guest does not have; anything else you run on
  the host's loopback is reachable from a desk.

## Layout

```
cmd/hollow            the CLI and the host (hollow serve)
cmd/hollow-agent      the program inside a desk
api                   the wire types, shared by host, agent and clients
client                a Go client for the API
internal/host         the daemon: token, desks, the API server, the embedded agent
internal/image        golden images: download, verify, provision, keep
internal/backend      the hypervisor interface, and qemu/ behind it
internal/agent        what the agent does: capture, input, exec, files, record
dist                  the systemd unit
```
