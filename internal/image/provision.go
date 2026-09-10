package image

// The Linux desk, described to cloud-init as a shell script.
//
// It runs once, on the golden image's only boot, and turns a stock Alpine
// cloud image into a desk: a virtual display, a window manager, a browser,
// the tools the agent shells out to, one unprivileged user to be the bot, and
// a service that fetches the agent from the host at every boot. The result
// is powered off and every desk is a copy-on-write clone of it.
//
// Alpine, because it is the smallest thing that has a package for everything
// here. Idle, the whole desk is well under a hundred megabytes; the browser
// is what costs memory, and the browser would cost the same anywhere.
const linuxProvision = `#!/bin/sh
cat > /root/hollow-provision.sh <<'BODY'
set -eu
echo "hollow: provisioning alpine $(cat /etc/alpine-release)"

if ! grep -q '^[^#].*/community' /etc/apk/repositories; then
	v=$(cut -d. -f1,2 /etc/alpine-release)
	echo "https://dl-cdn.alpinelinux.org/alpine/v$v/community" >> /etc/apk/repositories
fi
echo "hollow: installing packages"
apk update
apk add --no-cache xvfb xdotool openbox xterm ffmpeg chromium \
	font-dejavu font-noto font-noto-emoji dbus doas sudo curl \
	xsetroot xrandr xset xdpyinfo mesa-dri-gallium

echo "hollow: creating the bot user"
if ! id bot >/dev/null 2>&1; then
	adduser -D -s /bin/sh bot
fi
echo "bot ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/bot
chmod 440 /etc/sudoers.d/bot
mkdir -p /etc/doas.d
echo "permit nopass bot" > /etc/doas.d/bot.conf

echo "hollow: installing the session"
cat > /usr/local/bin/hollow-session <<'SESSION'
#!/bin/sh
# hollow-session: bring up the display, fetch the agent from the host, run it.
set -u
if [ -f /etc/hollow/desk.env ]; then . /etc/hollow/desk.env; fi
: "${HOLLOW_HOST:=10.0.2.2}"
: "${HOLLOW_PORT:=7070}"
: "${HOLLOW_WIDTH:=1280}"
: "${HOLLOW_HEIGHT:=800}"
export DISPLAY=:0
export HOME=/home/bot
export XDG_RUNTIME_DIR=/tmp/xdg-bot
mkdir -p "$HOME/bin" "$XDG_RUNTIME_DIR"
chmod 700 "$XDG_RUNTIME_DIR"
cd "$HOME"

Xvfb :0 -screen 0 "${HOLLOW_WIDTH}x${HOLLOW_HEIGHT}x24" -ac -nolisten tcp -noreset >/tmp/xvfb.log 2>&1 &
i=0
while ! xdpyinfo >/dev/null 2>&1; do
	i=$((i+1)); [ "$i" -gt 100 ] && break
	sleep 0.1
done
xsetroot -solid '#1b1e2b' || true
openbox >/tmp/openbox.log 2>&1 &

url="http://$HOLLOW_HOST:$HOLLOW_PORT/v1/agent/hollow-agent"
while ! curl -fsS --max-time 30 "$url" -o "$HOME/bin/hollow-agent.tmp"; do
	echo "waiting for $url"
	sleep 1
done
mv "$HOME/bin/hollow-agent.tmp" "$HOME/bin/hollow-agent"
chmod 755 "$HOME/bin/hollow-agent"
exec "$HOME/bin/hollow-agent" -listen 0.0.0.0:7000 -display :0
SESSION
chmod 755 /usr/local/bin/hollow-session

cat > /etc/init.d/hollow <<'SERVICE'
#!/sbin/openrc-run
description="hollow desk: display, window manager, and the agent"
command="/usr/local/bin/hollow-session"
command_user="bot:bot"
command_background="yes"
pidfile="/run/hollow-session.pid"
output_log="/var/log/hollow-session.log"
error_log="/var/log/hollow-session.log"

depend() {
	need localmount
	after bootmisc networking dbus
}

start_pre() {
	mkdir -p /mnt/hollow /etc/hollow /tmp/.X11-unix
	chmod 1777 /tmp/.X11-unix
	for dev in /dev/sr0 /dev/sr1 /dev/vdb /dev/vdc; do
		[ -e "$dev" ] || continue
		if mount -t iso9660 -o ro "$dev" /mnt/hollow 2>/dev/null; then
			if [ -f /mnt/hollow/hollow.env ]; then
				cp /mnt/hollow/hollow.env /etc/hollow/desk.env
				chmod 644 /etc/hollow/desk.env
				umount /mnt/hollow
				break
			fi
			umount /mnt/hollow
		fi
	done
	touch /var/log/hollow-session.log
	chown bot:bot /var/log/hollow-session.log
	return 0
}
SERVICE
chmod 755 /etc/init.d/hollow
rc-update add hollow default
rc-update add dbus default || true

echo "hollow: retiring cloud-init"
mkdir -p /etc/cloud
touch /etc/cloud/cloud-init.disabled
echo "hollow: done"
BODY

if sh /root/hollow-provision.sh > /root/hollow-provision.log 2>&1; then
	echo "HOLLOW-PROVISION-OK" > /dev/ttyS0
else
	echo "HOLLOW-PROVISION-FAILED (see /root/hollow-provision.log)" > /dev/ttyS0
	tail -n 40 /root/hollow-provision.log > /dev/ttyS0
fi
sleep 1
poweroff
`

const linuxMetaData = "instance-id: hollow-golden\nlocal-hostname: hollow\n"
