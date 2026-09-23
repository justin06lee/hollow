package host

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// meminfo reads total and available memory in MB from /proc/meminfo. Zero
// when there is no such file, which means "unknown", not "none".
func meminfo() (total, avail int) {
	if runtime.GOOS != "linux" {
		return 0, 0
	}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb / 1024
		case "MemAvailable:":
			avail = kb / 1024
		}
	}
	return total, avail
}
