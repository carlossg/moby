package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/go-units"
	"golang.org/x/net/context"
)

type containerStats struct {
	Name             string
	CPUPercentage    float64
	Memory           float64 // On Windows this is the private working set
	MemoryLimit      float64 // Not used on Windows
	MemoryPercentage float64 // Not used on Windows
	NetworkRx        float64
	NetworkTx        float64
	BlockRead        float64
	BlockWrite       float64
	PidsCurrent      uint64 // Not used on Windows
	mu               sync.Mutex
	err              error
}

type stats struct {
	mu     sync.Mutex
	ostype string
	cs     []*containerStats
}

// daemonOSType is set once we have at least one stat for a container
// from the daemon. It is used to ensure we print the right header based
// on the daemon platform.
var daemonOSType string

func (s *stats) add(cs *containerStats) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.isKnownContainer(cs.Name); !exists {
		s.cs = append(s.cs, cs)
		return true
	}
	return false
}

func (s *stats) remove(id string) {
	s.mu.Lock()
	if i, exists := s.isKnownContainer(id); exists {
		s.cs = append(s.cs[:i], s.cs[i+1:]...)
	}
	s.mu.Unlock()
}

func (s *stats) isKnownContainer(cid string) (int, bool) {
	for i, c := range s.cs {
		if c.Name == cid {
			return i, true
		}
	}
	return -1, false
}

func (s *containerStats) Collect(ctx context.Context, cli client.APIClient, streamStats bool, waitFirst *sync.WaitGroup) {
	logrus.Debugf("collecting stats for %s", s.Name)
	var (
		getFirst       bool
		previousCPU    uint64
		previousSystem uint64
		u              = make(chan error, 1)
	)

	defer func() {
		// if error happens and we get nothing of stats, release wait group whatever
		if !getFirst {
			getFirst = true
			waitFirst.Done()
		}
	}()

	response, err := cli.ContainerStats(ctx, s.Name, streamStats)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return
	}
	defer response.Body.Close()

	dec := json.NewDecoder(response.Body)
	go func() {
		for {
			var (
				v                 *types.StatsJSON
				memPercent        = 0.0
				cpuPercent        = 0.0
				blkRead, blkWrite uint64 // Only used on Linux
				mem               = 0.0
			)

			if err := dec.Decode(&v); err != nil {
				dec = json.NewDecoder(io.MultiReader(dec.Buffered(), response.Body))
				u <- err
				if err == io.EOF {
					break
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}

			daemonOSType = response.OSType

			if daemonOSType != "windows" {
				// MemoryStats.Limit will never be 0 unless the container is not running and we haven't
				// got any data from cgroup
				if v.MemoryStats.Limit != 0 {
					memPercent = float64(v.MemoryStats.Usage) / float64(v.MemoryStats.Limit) * 100.0
				}
				previousCPU = v.PreCPUStats.CPUUsage.TotalUsage
				previousSystem = v.PreCPUStats.SystemUsage
				cpuPercent = calculateCPUPercentUnix(previousCPU, previousSystem, v)
				blkRead, blkWrite = calculateBlockIO(v.BlkioStats)
				mem = float64(v.MemoryStats.Usage)

			} else {
				cpuPercent = calculateCPUPercentWindows(v)
				blkRead = v.StorageStats.ReadSizeBytes
				blkWrite = v.StorageStats.WriteSizeBytes
				mem = float64(v.MemoryStats.PrivateWorkingSet)
			}

			s.mu.Lock()
			s.CPUPercentage = cpuPercent
			s.Memory = mem
			s.NetworkRx, s.NetworkTx = calculateNetwork(v.Networks)
			s.BlockRead = float64(blkRead)
			s.BlockWrite = float64(blkWrite)
			if daemonOSType != "windows" {
				s.MemoryLimit = float64(v.MemoryStats.Limit)
				s.MemoryPercentage = memPercent
				s.PidsCurrent = v.PidsStats.Current
			}
			s.mu.Unlock()
			u <- nil
			if !streamStats {
				return
			}
		}
	}()
	for {
		select {
		case <-time.After(2 * time.Second):
			// zero out the values if we have not received an update within
			// the specified duration.
			s.mu.Lock()
			s.CPUPercentage = 0
			s.Memory = 0
			s.MemoryPercentage = 0
			s.MemoryLimit = 0
			s.NetworkRx = 0
			s.NetworkTx = 0
			s.BlockRead = 0
			s.BlockWrite = 0
			s.PidsCurrent = 0
			s.err = errors.New("timeout waiting for stats")
			s.mu.Unlock()
			// if this is the first stat you get, release WaitGroup
			if !getFirst {
				getFirst = true
				waitFirst.Done()
			}
		case err := <-u:
			if err != nil {
				s.mu.Lock()
				s.err = err
				s.mu.Unlock()
				continue
			}
			s.err = nil
			// if this is the first stat you get, release WaitGroup
			if !getFirst {
				getFirst = true
				waitFirst.Done()
			}
		}
		if !streamStats {
			return
		}
	}
}

func (s *containerStats) Display(w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if daemonOSType == "windows" {
		// NOTE: if you change this format, you must also change the err format below!
		format := "%s\t%.2f%%\t%s\t%s / %s\t%s / %s\n"
		if s.err != nil {
			format = "%s\t%s\t%s\t%s / %s\t%s / %s\n"
			errStr := "--"
			fmt.Fprintf(w, format,
				s.Name, errStr, errStr, errStr, errStr, errStr, errStr,
			)
			err := s.err
			return err
		}
		fmt.Fprintf(w, format,
			s.Name,
			s.CPUPercentage,
			units.BytesSize(s.Memory),
			units.HumanSizeWithPrecision(s.NetworkRx, 3), units.HumanSizeWithPrecision(s.NetworkTx, 3),
			units.HumanSizeWithPrecision(s.BlockRead, 3), units.HumanSizeWithPrecision(s.BlockWrite, 3))
	} else {
		// NOTE: if you change this format, you must also change the err format below!
		format := "%s\t%.2f%%\t%s / %s\t%.2f%%\t%s / %s\t%s / %s\t%d\n"
		if s.err != nil {
			format = "%s\t%s\t%s / %s\t%s\t%s / %s\t%s / %s\t%s\n"
			errStr := "--"
			fmt.Fprintf(w, format,
				s.Name, errStr, errStr, errStr, errStr, errStr, errStr, errStr, errStr, errStr,
			)
			err := s.err
			return err
		}
		fmt.Fprintf(w, format,
			s.Name,
			s.CPUPercentage,
			units.BytesSize(s.Memory), units.BytesSize(s.MemoryLimit),
			s.MemoryPercentage,
			units.HumanSizeWithPrecision(s.NetworkRx, 3), units.HumanSizeWithPrecision(s.NetworkTx, 3),
			units.HumanSizeWithPrecision(s.BlockRead, 3), units.HumanSizeWithPrecision(s.BlockWrite, 3),
			s.PidsCurrent)
	}
	return nil
}

func calculateCPUPercentUnix(previousCPU, previousSystem uint64, v *types.StatsJSON) float64 {
	var (
		cpuPercent = 0.0
		// calculate the change for the cpu usage of the container in between readings
		cpuDelta = float64(v.CPUStats.CPUUsage.TotalUsage) - float64(previousCPU)
		// calculate the change for the entire system between readings
		systemDelta = float64(v.CPUStats.SystemUsage) - float64(previousSystem)
	)

	if systemDelta > 0.0 && cpuDelta > 0.0 {
		cpuPercent = (cpuDelta / systemDelta) * float64(len(v.CPUStats.CPUUsage.PercpuUsage)) * 100.0
	}
	return cpuPercent
}

func calculateCPUPercentWindows(v *types.StatsJSON) float64 {
	// Max number of 100ns intervals between the previous time read and now
	possIntervals := uint64(v.Read.Sub(v.PreRead).Nanoseconds()) // Start with number of ns intervals
	possIntervals /= 100                                         // Convert to number of 100ns intervals
	possIntervals *= uint64(v.NumProcs)                          // Multiple by the number of processors

	// Intervals used
	intervalsUsed := v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage

	// Percentage avoiding divide-by-zero
	if possIntervals > 0 {
		return float64(intervalsUsed) / float64(possIntervals) * 100.0
	}
	return 0.00
}

func calculateBlockIO(blkio types.BlkioStats) (blkRead uint64, blkWrite uint64) {
	for _, bioEntry := range blkio.IoServiceBytesRecursive {
		switch strings.ToLower(bioEntry.Op) {
		case "read":
			blkRead = blkRead + bioEntry.Value
		case "write":
			blkWrite = blkWrite + bioEntry.Value
		}
	}
	return
}

func calculateNetwork(network map[string]types.NetworkStats) (float64, float64) {
	var rx, tx float64

	for _, v := range network {
		rx += float64(v.RxBytes)
		tx += float64(v.TxBytes)
	}
	return rx, tx
}
