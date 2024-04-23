package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
)

var labelCname = []string{"container_name"}

type DockerCollector struct {
	cli *client.Client
}

func newDockerCollector() *DockerCollector {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Fatalf("can't create docker client: %v", err)
	}

	return &DockerCollector{
		cli: cli,
	}
}

func (c *DockerCollector) Describe(_ chan<- *prometheus.Desc) {

}

func (c *DockerCollector) Collect(ch chan<- prometheus.Metric) {
	containers, err := c.cli.ContainerList(context.Background(), container.ListOptions{
		All: true,
	})
	if err != nil {
		log.Error("can't list containers: ", err)
		return
	}

	var wg sync.WaitGroup

	for _, container := range containers {
		wg.Add(1)

		go c.processContainer(container, ch, &wg)
	}
	wg.Wait()
}

func (c *DockerCollector) processContainer(container types.Container, ch chan<- prometheus.Metric, wg *sync.WaitGroup) {
	defer wg.Done()
	cName := strings.TrimPrefix(strings.Join(container.Names, ";"), "/")
	var isRunning float64
	if container.State == "running" {
		isRunning = 1
	}

	// container state metric for all containers
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_container_running",
		"1 if docker container is running, 0 otherwise",
		labelCname,
		nil,
	), prometheus.GaugeValue, isRunning, cName)

	// stats metrics only for running containers
	if isRunning == 1 {

		if stats, err := c.cli.ContainerStats(context.Background(), container.ID, false); err != nil {
			log.Fatal(err)
		} else {
			var containerStats types.StatsJSON
			err := json.NewDecoder(stats.Body).Decode(&containerStats)
			if err != nil {
				log.Error("can't read api stats: ", err)
			}
			if err := stats.Body.Close(); err != nil {
				log.Error("can't close body: ", err)
			}

			c.blockIoMetrics(ch, &containerStats, cName)

			c.memoryMetrics(ch, &containerStats, cName)

			c.networkMetrics(ch, &containerStats, cName)

			c.CPUMetrics(ch, &containerStats, cName)

			c.pidsMetrics(ch, &containerStats, cName)
		}
	}
}

func (c *DockerCollector) CPUMetrics(ch chan<- prometheus.Metric, containerStats *types.StatsJSON, cName string) {
	totalUsage := containerStats.CPUStats.CPUUsage.TotalUsage
	cpuDelta := totalUsage - containerStats.PreCPUStats.CPUUsage.TotalUsage
	sysemDelta := containerStats.CPUStats.SystemUsage - containerStats.PreCPUStats.SystemUsage

	cpuUtilization := float64(cpuDelta) / float64(sysemDelta) * 100.0

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_cpu_utilization_percent",
		"CPU utilization in percent",
		labelCname,
		nil,
	), prometheus.GaugeValue, cpuUtilization, cName)

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_cpu_utilization_seconds_total",
		"Cumulative CPU utilization in seconds",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(totalUsage)/1e9, cName)
}

func (c *DockerCollector) networkMetrics(ch chan<- prometheus.Metric, containerStats *types.StatsJSON, cName string) {
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_network_rx_bytes",
		"Network received bytes total",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(containerStats.Networks["eth0"].RxBytes), cName)
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_network_tx_bytes",
		"Network sent bytes total",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(containerStats.Networks["eth0"].TxBytes), cName)
}

func (c *DockerCollector) memoryMetrics(ch chan<- prometheus.Metric, containerStats *types.StatsJSON, cName string) {
	// Note: An old version of this code subtracted the "cache" stat from the cgroup's memory usage.
	// However, this stat only exists for cgroup v1. cgroup v2 uses the "file" stat for the same value.
	// This lead to containerStats.MemoryStats.Stats["cache"] being the default value of 0 and therefore
	// effectively reporting the cgroup's memory usage including the disk cache of the kernel
	// (which can vastly overestimate the "true" memory usage in many cases).
	//
	// Actually, Docker (and cAdvisor and likely more) use total_inactive_file/inactive_file nowadays.
	// Although being (probably?) more precise when it comes to enforcing resources, I think it makes more
	// sense to use the effectively used memory usage like before (but fixed for cgroup v2).
	//
	// Further reading:
	//   - https://github.com/docker/cli/blob/26.1/cli/command/container/stats_helpers.go#L227-L249
	//   - https://docs.kernel.org/admin-guide/cgroup-v1/memory.html#stat-file
	//   - https://docs.kernel.org/admin-guide/cgroup-v2.html#memory-interface-files

	var kernelDiskCacheKeyName string
	_, isCgroupV1 := containerStats.MemoryStats.Stats["cache"]
	if isCgroupV1 {
		kernelDiskCacheKeyName = "cache"
	}
	_, isCgroupV2 := containerStats.MemoryStats.Stats["file"]
	if isCgroupV2 {
		kernelDiskCacheKeyName = "file"
	}

	if !isCgroupV1 && !isCgroupV2 {
		log.WithField("container", cName).Warn("could not find \"cache\" stat (cgroup v1) nor \"file\" stat (cgroup v2)")
	}

	memoryUsage := containerStats.MemoryStats.Usage - containerStats.MemoryStats.Stats[kernelDiskCacheKeyName]
	memoryTotal := containerStats.MemoryStats.Limit

	memoryUtilization := float64(memoryUsage) / float64(memoryTotal) * 100.0
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_memory_usage_bytes",
		"Total memory usage bytes",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(memoryUsage), cName)
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_memory_total_bytes",
		"Total memory bytes",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(memoryTotal), cName)
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_memory_utilization_percent",
		"Memory utilization percent",
		labelCname,
		nil,
	), prometheus.GaugeValue, memoryUtilization, cName)
}

func (c *DockerCollector) blockIoMetrics(ch chan<- prometheus.Metric, containerStats *types.StatsJSON, cName string) {
	var readTotal, writeTotal uint64
	for _, b := range containerStats.BlkioStats.IoServiceBytesRecursive {
		if strings.EqualFold(b.Op, "read") {
			readTotal += b.Value
		}
		if strings.EqualFold(b.Op, "write") {
			writeTotal += b.Value
		}
	}

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_block_io_read_bytes",
		"Block I/O read bytes",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(readTotal), cName)

	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_block_io_write_bytes",
		"Block I/O write bytes",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(writeTotal), cName)
}

func (c *DockerCollector) pidsMetrics(ch chan<- prometheus.Metric, containerStats *types.StatsJSON, cName string) {
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(
		"dex_pids_current",
		"Current number of pids in the cgroup",
		labelCname,
		nil,
	), prometheus.CounterValue, float64(containerStats.PidsStats.Current), cName)
}
