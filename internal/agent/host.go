package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/jester/sddb/internal/types"
)

type hostSampler struct {
	mu        sync.Mutex
	prevIdle  uint64
	prevTotal uint64
}

func (h *hostSampler) sample(containers []types.ContainerState) types.HostStats {
	var hs types.HostStats

	if cs, ok := readCPUStat(); ok {
		h.mu.Lock()
		if h.prevTotal > 0 {
			idleDelta := cs.idle - h.prevIdle
			totalDelta := cs.total - h.prevTotal
			if totalDelta > 0 {
				hs.CPUPercent = (1.0 - float64(idleDelta)/float64(totalDelta)) * 100
			}
		}
		h.prevIdle = cs.idle
		h.prevTotal = cs.total
		h.mu.Unlock()
	}

	if ms, ok := readMemInfo(); ok {
		hs.MemUsage = ms.total - ms.available
		hs.MemTotal = ms.total
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		hs.DiskTotal = stat.Blocks * uint64(stat.Bsize)
		hs.DiskUsage = hs.DiskTotal - stat.Bfree*uint64(stat.Bsize)
	}

	for _, c := range containers {
		hs.NetRxRate += c.NetRxRate
		hs.NetTxRate += c.NetTxRate
	}

	return hs
}

type cpuStat struct {
	idle  uint64
	total uint64
}

func readCPUStat() (cpuStat, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuStat{}, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return cpuStat{}, false
		}
		var vals [10]uint64
		for i := 1; i < len(fields) && i-1 < 10; i++ {
			vals[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
		}
		idle := vals[3] + vals[4] // idle + iowait
		var total uint64
		for _, v := range vals {
			total += v
		}
		return cpuStat{idle: idle, total: total}, true
	}
	return cpuStat{}, false
}

type memInfo struct {
	total     uint64
	available uint64
}

func readMemInfo() (memInfo, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return memInfo{}, false
	}
	defer f.Close()

	var mi memInfo
	found := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() && found < 2 {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		val, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			mi.total = val * 1024
			found++
		case "MemAvailable:":
			mi.available = val * 1024
			found++
		}
	}
	return mi, found == 2
}
