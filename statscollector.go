package main

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	binrpc "github.com/florentchauveau/go-kamailio-binrpc/v2"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"gopkg.in/urfave/cli.v1"
)

// declare a series of prometheus metric descriptions
// we can reuse them for each scrape
var (
	constLabels prometheus.Labels
	core_request_total, core_rcv_request_total, core_reply_total, core_rcv_reply_total,
	shmem_bytes, shmem_fragments, pkgmem_bytes,
	dns_failed,
	bad_uri, bad_msg_hdr,
	sl_reply_total, sl_type_total,
	tcp_total, tcp_connections, tcp_writequeue,
	tmx_code_total, tmx_type_total, tmx, tmx_rpl_total,
	dialog *prometheus.Desc
)

func initMetrics(labels prometheus.Labels) {
	constLabels = labels

	core_request_total = prometheus.NewDesc(
		"kamailio_core_request_total",
		"Request counters",
		[]string{"method"}, constLabels)

	core_rcv_request_total = prometheus.NewDesc(
		"kamailio_core_rcv_request_total",
		"Received requests by method",
		[]string{"method"}, constLabels)

	core_reply_total = prometheus.NewDesc(
		"kamailio_core_reply_total",
		"Reply counters",
		[]string{"type"}, constLabels)

	core_rcv_reply_total = prometheus.NewDesc(
		"kamailio_core_rcv_reply_total",
		"Received replies by code",
		[]string{"code"}, constLabels)

	shmem_bytes = prometheus.NewDesc(
		"kamailio_shm_bytes",
		"Shared memory sizes",
		[]string{"type"}, constLabels)

	shmem_fragments = prometheus.NewDesc(
		"kamailio_shm_fragments",
		"Shared memory fragment count",
		[]string{}, constLabels)

	pkgmem_bytes = prometheus.NewDesc(
		"kamailio_pkg_bytes",
		"Private memory",
		[]string{"index", "pid", "rank", "type"}, constLabels)

	dns_failed = prometheus.NewDesc(
		"kamailio_dns_failed_request_total",
		"Failed dns requests",
		[]string{}, constLabels)

	bad_uri = prometheus.NewDesc(
		"kamailio_bad_uri_total",
		"Messages with bad uri",
		[]string{}, constLabels)

	bad_msg_hdr = prometheus.NewDesc(
		"kamailio_bad_msg_hdr",
		"Messages with bad message header",
		[]string{}, constLabels)

	sl_reply_total = prometheus.NewDesc(
		"kamailio_sl_reply_total",
		"Stateless replies by code",
		[]string{"code"}, constLabels)

	sl_type_total = prometheus.NewDesc(
		"kamailio_sl_type_total",
		"Stateless replies by type",
		[]string{"type"}, constLabels)

	tcp_total = prometheus.NewDesc(
		"kamailio_tcp_total",
		"TCP connection counters",
		[]string{"type"}, constLabels)

	tcp_connections = prometheus.NewDesc(
		"kamailio_tcp_connections",
		"Opened TCP connections",
		[]string{}, constLabels)

	tcp_writequeue = prometheus.NewDesc(
		"kamailio_tcp_writequeue",
		"TCP write queue size",
		[]string{}, constLabels)

	tmx_code_total = prometheus.NewDesc(
		"kamailio_tmx_code_total",
		"Completed Transaction counters by code",
		[]string{"code"}, constLabels)

	tmx_type_total = prometheus.NewDesc(
		"kamailio_tmx_type_total",
		"Completed Transaction counters by type",
		[]string{"type"}, constLabels)

	tmx = prometheus.NewDesc(
		"kamailio_tmx",
		"Ongoing Transactions",
		[]string{"type"}, constLabels)

	tmx_rpl_total = prometheus.NewDesc(
		"kamailio_tmx_rpl_total",
		"Tmx reply counters",
		[]string{"type"}, constLabels)

	dialog = prometheus.NewDesc(
		"kamailio_dialog",
		"Ongoing Dialogs",
		[]string{"type"}, constLabels)

	return
}

// MemoryEntry is a struct to store PKG memory values
type MemoryEntry struct {
	Entry      string
	Pid        string
	Rank       string
	Used       string
	Free       string
	RealUsed   string
	TotalSize  string
	TotalFrags string
}

// the actual Collector object
type StatsCollector struct {
	cliContext   *cli.Context
	socketPath   string
	kamailioHost string
	kamailioPort int
}

// produce a new StatsCollector object
func NewStatsCollector(cliContext *cli.Context) (*StatsCollector, error) {

	// fill the Collector struct
	collector := &StatsCollector{
		cliContext:   cliContext,
		socketPath:   cliContext.String("socketPath"),
		kamailioHost: cliContext.String("host"),
		kamailioPort: cliContext.Int("port"),
	}

	// fine, return the created object struct
	return collector, nil
}

// part of the prometheus.Collector interface
func (c *StatsCollector) Describe(descriptionChannel chan<- *prometheus.Desc) {
	// DescribeByCollect is a helper to implement the Describe method of a custom
	// Collector. It collects the metrics from the provided Collector and sends
	// their descriptors to the provided channel.
	prometheus.DescribeByCollect(c, descriptionChannel)
}

// part of the prometheus.Collector interface
func (c *StatsCollector) Collect(metricChannel chan<- prometheus.Metric) {

	// read PKG memory stats from Kamailio
	log.Debug("Fetching PKG memory stats....")
	memStatMap, err := c.fetchPKGMemStats()
	if err == nil {
		// and produce prometheus.Metric
		log.Debug("Pushing metrics of memory stats....")
		producePKGMemMetrics(memStatMap, metricChannel)
	} else {
		// something went wrong
		// TODO: add a error metric
		log.Error("Could not fetch PKG memory stats values from kamailio: ", err)
	}

	// read all stats from Kamailio
	log.Debug("Fetching stats from statistics module....")
	completeStatMap, err := c.fetchStats()
	if err == nil {
		log.Debug("Pushing metrics of standard stats....")
		// and produce various prometheus.Metric for well-known stats
		produceMetrics(completeStatMap, metricChannel)
		log.Debug("Pushing metrics of scripted stats....")
		// produce prometheus.Metric objects for scripted stats (if any)
		convertScriptedMetrics(completeStatMap, metricChannel)
	} else {
		// something went wrong
		// TODO: add a error metric
		log.Error("Could not fetch values from kamailio: ", err)
	}

}

func (c *StatsCollector) execRPCCommand(command string, param string) ([]binrpc.Record, error) {
	log.Debug("Gonna send request following command to Kamailio: ", command, param)

	// establish connection to Kamailio server
	var err error
	var conn net.Conn
	if c.kamailioHost == "" {
		log.Debug("Requesting stats from kamailio via domain socket ", c.socketPath)
		conn, err = net.Dial("unix", c.socketPath)
	} else {
		address := fmt.Sprintf("%s:%d", c.kamailioHost, c.kamailioPort)
		log.Debug("Requesting stats from kamailio via binrpc ", address)
		conn, err = net.Dial("tcp", address)
	}
	if err != nil {
		log.Error("Error while establishing connection to Kamailio")
		return nil, err
	}
	defer conn.Close()
	// TODO
	// c.conn.SetDeadline(time.Now().Add(c.Timeout))

	// WritePacket returns the cookie generated
	var cookie []byte
	if param == "" {
		cookie, err = binrpc.WritePacket(conn, command)
	} else {
		cookie, err = binrpc.WritePacket(conn, command, param)
	}
	if err != nil {
		log.Error("Error while sending request to Kamailio")
		return nil, err
	}

	// the cookie is passed again for verification
	// we receive records in response
	records, err := binrpc.ReadPacket(conn, cookie)
	if err != nil {
		log.Error("Error while reading response from Kamailio")
		return nil, err
	}

	log.Debug("Got response from Kamailio for request: ", command)
	return records, nil
}

func (c *StatsCollector) fetchPKGMemStats() ([]MemoryEntry, error) {
	records, err := c.execRPCCommand("pkg.stats", "")
	if err != nil {
		return nil, err
	}

	log.Debugf("Got %d records", len(records))
	// convert the structure into a MemoryEntry
	var result = []MemoryEntry{}

	for _, record := range records {
		m := MemoryEntry{}
		// a record is a memory entry
		items, _ := record.StructItems()
		log.Debugf("Got %d items for this record", len(items))
		for _, item := range items {
			log.Debugf("Dealing with key: %s", item.Key)
			intRes, err := item.Value.Int()
			if err != nil {
				log.Errorf("Was not able to convert value to string for key: %s. Ignoring.", item.Key)
				continue
			}
			value := strconv.Itoa(intRes)
			switch item.Key {
			case "entry":
				m.Entry = value
			case "pid":
				m.Pid = value
			case "rank":
				m.Rank = value
			case "used":
				m.Used = value
			case "free":
				m.Free = value
			case "real_used":
				m.RealUsed = value
			case "total_size":
				m.TotalSize = value
			case "total_frags":
				m.TotalFrags = value
			}
		}
		result = append(result, m)
	}

	return result, nil
}

// connect to Kamailio and perform a "stats.fetch" rpc call
// result is a flat key=>value map
func (c *StatsCollector) fetchStats() (map[string]string, error) {

	// TODO measure rpc time
	//timer := prometheus.NewTimer(rpc_request_duration)
	//defer timer.ObserveDuration()

	records, err := c.execRPCCommand("stats.fetch", "all")
	if err != nil {
		return nil, err
	}

	log.Debugf("Mapping received response: %s", records)
	// convert the structure into a simple key=>value map
	items, _ := records[0].StructItems()
	result := make(map[string]string)
	for _, item := range items {
		value, _ := item.Value.String()
		result[item.Key] = value
	}

	return result, nil
}

func producePKGMemMetrics(memStatMap []MemoryEntry, metricChannel chan<- prometheus.Metric) {

	for _, e := range memStatMap {
		//Labels
		labelValues := []string{
			// entry - pid - rank - type
			e.Entry,
			e.Pid,
			e.Rank,
		}
		labelUsed := append(labelValues, "used")
		labelFree := append(labelValues, "free")
		labelRealUsed := append(labelValues, "real_used")
		labelTotal := append(labelValues, "total")
		labelFrags := append(labelValues, "total_frags")

		//Values
		convertPKGStatToMetric(e.Used, labelUsed, metricChannel)
		convertPKGStatToMetric(e.Free, labelFree, metricChannel)
		convertPKGStatToMetric(e.RealUsed, labelRealUsed, metricChannel)
		convertPKGStatToMetric(e.TotalSize, labelTotal, metricChannel)
		convertPKGStatToMetric(e.TotalFrags, labelFrags, metricChannel)

	}
}

func convertPKGStatToMetric(value string, labels []string, metricChannel chan<- prometheus.Metric) {
	if valueFloat, err := strconv.ParseFloat(value, 64); err == nil {
		metric, err := prometheus.NewConstMetric(
			pkgmem_bytes,
			prometheus.GaugeValue,
			valueFloat,
			labels...,
		)
		if err == nil {
			// handover the metric to prometheus api
			metricChannel <- metric
		} else {
			// or skip and complain
			log.Warnf("Could not convert stat value [%s]: %s", value, err)
		}
	}
}

// produce a series of prometheus.Metric values by converting "well-known" prometheus stats
func produceMetrics(completeStatMap map[string]string, metricChannel chan<- prometheus.Metric) {

	// kamailio_core_request_total
	convertStatToMetric(completeStatMap, "core.drop_requests", "drop", core_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.err_requests", "err", core_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.fwd_requests", "fwd", core_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests", "rcv", core_request_total, metricChannel, prometheus.CounterValue)

	// kamailio_core_rcv_request_total
	convertStatToMetric(completeStatMap, "core.rcv_requests_ack", "ack", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_bye", "bye", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_cancel", "cancel", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_info", "info", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_invite", "invite", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_message", "message", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_notify", "notify", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_options", "options", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_prack", "prack", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_publish", "publish", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_refer", "refer", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_register", "register", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_subscribe", "subscribe", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_requests_update", "update", core_rcv_request_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.unsupported_methods", "unsupported", core_rcv_request_total, metricChannel, prometheus.CounterValue)

	// kamailio_core_reply_total
	convertStatToMetric(completeStatMap, "core.drop_replies", "drop", core_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.err_replies", "err", core_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.fwd_replies", "fwd", core_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies", "rcv", core_reply_total, metricChannel, prometheus.CounterValue)

	// kamailio_core_rcv_reply_total
	convertStatToMetric(completeStatMap, "core.rcv_replies_18x", "18x", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_1xx", "1xx", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_2xx", "2xx", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_3xx", "3xx", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_401", "401", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_404", "404", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_407", "407", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_408", "408", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_480", "480", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_486", "486", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_4xx", "4xx", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_5xx", "5xx", core_rcv_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.rcv_replies_6xx", "6xx", core_rcv_reply_total, metricChannel, prometheus.CounterValue)

	// kamailio_shm_bytes
	convertStatToMetric(completeStatMap, "shmem.free_size", "free", shmem_bytes, metricChannel, prometheus.GaugeValue)
	convertStatToMetric(completeStatMap, "shmem.max_used_size", "max_used", shmem_bytes, metricChannel, prometheus.GaugeValue)
	convertStatToMetric(completeStatMap, "shmem.real_used_size", "real_used", shmem_bytes, metricChannel, prometheus.GaugeValue)
	convertStatToMetric(completeStatMap, "shmem.total_size", "total", shmem_bytes, metricChannel, prometheus.GaugeValue)
	convertStatToMetric(completeStatMap, "shmem.used_size", "used", shmem_bytes, metricChannel, prometheus.GaugeValue)

	convertStatToMetric(completeStatMap, "shmem.fragments", "", shmem_fragments, metricChannel, prometheus.GaugeValue)
	convertStatToMetric(completeStatMap, "dns.failed_dns_request", "", dns_failed, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.bad_URIs_rcvd", "", bad_uri, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "core.bad_msg_hdr", "", bad_msg_hdr, metricChannel, prometheus.CounterValue)

	// kamailio_sl_reply_total
	convertStatToMetric(completeStatMap, "sl.1xx_replies", "1xx", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.200_replies", "200", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.202_replies", "202", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.2xx_replies", "2xx", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.300_replies", "300", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.301_replies", "301", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.302_replies", "302", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.3xx_replies", "3xx", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.400_replies", "400", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.401_replies", "401", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.403_replies", "403", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.404_replies", "404", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.407_replies", "407", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.408_replies", "408", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.483_replies", "483", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.4xx_replies", "4xx", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.500_replies", "500", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.5xx_replies", "5xx", sl_reply_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.6xx_replies", "6xx", sl_reply_total, metricChannel, prometheus.CounterValue)

	// kamailio_sl_type_total
	convertStatToMetric(completeStatMap, "sl.failures", "failure", sl_type_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.received_ACKs", "received_ack", sl_type_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.sent_err_replies", "sent_err_reply", sl_type_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.sent_replies", "sent_reply", sl_type_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "sl.xxx_replies", "xxx_reply", sl_type_total, metricChannel, prometheus.CounterValue)

	// kamailio_tcp_total
	convertStatToMetric(completeStatMap, "tcp.con_reset", "con_reset", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.con_timeout", "con_timeout", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.connect_failed", "connect_failed", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.connect_success", "connect_success", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.established", "established", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.local_reject", "local_reject", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.passive_open", "passive_open", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.send_timeout", "send_timeout", tcp_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tcp.sendq_full", "sendq_full", tcp_total, metricChannel, prometheus.CounterValue)
	// kamailio_tcp_connections
	convertStatToMetric(completeStatMap, "tcp.current_opened_connections", "", tcp_connections, metricChannel, prometheus.GaugeValue)
	// kamailio_tcp_writequeue
	convertStatToMetric(completeStatMap, "tcp.current_write_queue_size", "", tcp_writequeue, metricChannel, prometheus.GaugeValue)

	// kamailio_tmx_code_total
	convertStatToMetric(completeStatMap, "tmx.2xx_transactions", "2xx", tmx_code_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.3xx_transactions", "3xx", tmx_code_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.4xx_transactions", "4xx", tmx_code_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.5xx_transactions", "5xx", tmx_code_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.6xx_transactions", "6xx", tmx_code_total, metricChannel, prometheus.CounterValue)
	// kamailio_tmx_type_total
	convertStatToMetric(completeStatMap, "tmx.UAC_transactions", "uac", tmx_type_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.UAS_transactions", "uas", tmx_type_total, metricChannel, prometheus.CounterValue)
	// kamailio_tmx
	convertStatToMetric(completeStatMap, "tmx.active_transactions", "active", tmx, metricChannel, prometheus.GaugeValue)
	convertStatToMetric(completeStatMap, "tmx.inuse_transactions", "inuse", tmx, metricChannel, prometheus.GaugeValue)

	// kamailio_tmx_rpl_total
	convertStatToMetric(completeStatMap, "tmx.rpl_absorbed", "absorbed", tmx_rpl_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.rpl_generated", "generated", tmx_rpl_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.rpl_received", "received", tmx_rpl_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.rpl_relayed", "relayed", tmx_rpl_total, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "tmx.rpl_sent", "sent", tmx_rpl_total, metricChannel, prometheus.CounterValue)

	// kamailio_dialog
	convertStatToMetric(completeStatMap, "dialog.active_dialogs", "active_dialogs", dialog, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "dialog.early_dialogs", "early_dialogs", dialog, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "dialog.expired_dialogs", "expired_dialogs", dialog, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "dialog.failed_dialogs", "failed_dialogs", dialog, metricChannel, prometheus.CounterValue)
	convertStatToMetric(completeStatMap, "dialog.processed_dialogs", "processed_dialogs", dialog, metricChannel, prometheus.CounterValue)
}

// Iterate all reported "stats" keys and find those with a prefix of "script."
// These values are user-defined and populated within the kamailio script.
// See https://www.kamailio.org/docs/modules/5.2.x/modules/statistics.html
func convertScriptedMetrics(data map[string]string, prom chan<- prometheus.Metric) {
	for k := range data {
		// k = "script.custom_total"
		if strings.HasPrefix(k, "script.") {
			// metricName = "custom_total"
			metricName := strings.TrimPrefix(k, "script.")
			metricName = strings.ToLower(metricName)
			var valueType prometheus.ValueType
			// deduce the metrics value type by following https://prometheus.io/docs/practices/naming/
			if strings.HasSuffix(k, "_total") || strings.HasSuffix(k, "_seconds") || strings.HasSuffix(k, "_bytes") {
				valueType = prometheus.CounterValue
			} else {
				valueType = prometheus.GaugeValue
			}
			// create a metric description on the fly
			description := prometheus.NewDesc("kamailio_"+metricName, "Scripted metric "+metricName, []string{}, constLabels)
			// and produce a metric
			convertStatToMetric(data, k, "", description, prom, valueType)
		}
	}
}

// convert a single "stat" value to a prometheus metric
// invalid "stat" paires are skipped but logged
func convertStatToMetric(completeStatMap map[string]string, statKey string, optionalLabelValue string, metricDescription *prometheus.Desc, metricChannel chan<- prometheus.Metric, valueType prometheus.ValueType) {
	// check wether we got a labelValue or not
	var labelValues []string
	if optionalLabelValue != "" {
		labelValues = []string{optionalLabelValue}
	} else {
		labelValues = []string{}
	}
	// get the stat-value ...
	if valueAsString, ok := completeStatMap[statKey]; ok {
		// ... convert it to a float
		if value, err := strconv.ParseFloat(valueAsString, 64); err == nil {
			// and produce a prometheus metric
			metric, err := prometheus.NewConstMetric(
				metricDescription,
				valueType,
				value,
				labelValues...,
			)
			if err == nil {
				// handover the metric to prometheus api
				metricChannel <- metric
			} else {
				// or skip and complain
				log.Warnf("Could not convert stat value [%s]: %s", statKey, err)
			}
		}
	} else {
		// skip stat values not found in completeStatMap
		// can happen if some kamailio modules are not loaded
		// and thus certain stat entries are not created
		log.Debugf("Skipping stat value [%s], it was not returned by kamailio", statKey)
	}
}
