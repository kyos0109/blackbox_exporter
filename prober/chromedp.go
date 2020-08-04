package prober

import (
	"context"
	"io/ioutil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/blackbox_exporter/config"

	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type MonitorInfo struct {
	URL                   *url.URL
	Logger                *log.Logger
	EventLoadingFailed    *network.EventLoadingFailed
	EventResponseReceived *network.EventResponseReceived
	CdpLog                *cdplog.EventEntryAdded
	ResponseLatencyMS     float64
	WaitGroup             sync.WaitGroup
}

func runChromedpLocal(pctx context.Context, headless bool) (context.Context, context.CancelFunc) {
	dir, err := ioutil.TempDir("", "chromedp-wow")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("headless", headless),
		chromedp.Flag("ignore-certificate-errors", false),
		chromedp.UserDataDir(dir),
	)

	return chromedp.NewExecAllocator(pctx, opts...)
}

func runChromedpRemote(pctx context.Context, ws *string) (context.Context, context.CancelFunc) {
	return chromedp.NewRemoteAllocator(pctx, *ws)
}

func ProbeChromedp(ctx context.Context, target string, module config.Module, registry *prometheus.Registry, logger log.Logger) (success bool) {
	var (
		cancel context.CancelFunc
		m      MonitorInfo
		err    error

		requestList = make(map[string]string)

		durationGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_requests_duration_seconds",
			Help: "Duration of http request by phase, summed over all redirects",
		}, []string{"URL", "ServerIP", "StatusCode"})
		errorStatusGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_error",
			Help: "http response error code by url",
		}, []string{"URL", "ServerIP", "StatusCode"})
		domContentLatencyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "probe_http_dom_content_seconds",
			Help: "http dom content response latency",
		})
		loadContentLatencyGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "probe_http_page_load_seconds",
			Help: "http page load content response latency",
		})
		httpRequestCountter = prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "probe_http_request_count",
			Help: "http request total count",
		}, []string{"Method"})
		otherErrorGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_other_error",
			Help: "http response other error message",
		}, []string{"URL", "BlockedReason", "Reason", "Type", "ReqURL"})
		consoleMessageGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_chrome_console_message",
			Help: "chrome console message",
		}, []string{"Level", "Source", "Message", "URL"})
	)

	registry.MustRegister(durationGaugeVec)
	registry.MustRegister(errorStatusGaugeVec)
	registry.MustRegister(domContentLatencyGauge)
	registry.MustRegister(loadContentLatencyGauge)
	registry.MustRegister(httpRequestCountter)
	registry.MustRegister(otherErrorGaugeVec)
	registry.MustRegister(consoleMessageGaugeVec)

	cdpConfig := module.Chromedp

	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		target = "http://" + target
	}

	m.URL, err = url.Parse(target)
	if err != nil {
		level.Error(logger).Log("msg", "Could not parse target URL", "err", err)
		return false
	}

	webSocketURL := cdpConfig.ChromedpWS

	level.Info(logger).Log("Chromedp_Wait_Delay", cdpConfig.ChromedpWaitTime)

	switch cdpConfig.Remote && len(webSocketURL) > 3 {
	case true:
		level.Info(logger).Log("Use remote Chrome ->", webSocketURL)
		ctx, cancel = runChromedpRemote(ctx, &webSocketURL)
		defer cancel()
	case false:
		level.Info(logger).Log("Use local Chrome")
		ctx, cancel = runChromedpLocal(ctx, cdpConfig.Headless)
		defer cancel()
	default:
		level.Error(logger).Log("Unknown Remote Option.")
		return false
	}

	ctx, cancel = chromedp.NewContext(ctx)
	defer cancel()

	domTimer := prometheus.NewTimer(prometheus.ObserverFunc(domContentLatencyGauge.Set))
	loadTimer := prometheus.NewTimer(prometheus.ObserverFunc(loadContentLatencyGauge.Set))
	m.Logger = &logger
	m.ResponseLatencyMS = cdpConfig.ResponseLatencyMS

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch ev.(type) {
		case *network.EventResponseReceived:
			m.EventResponseReceived = ev.(*network.EventResponseReceived)
			m.WaitGroup.Add(1)
			go m.networkEventResponseReceived(
				cancel,
				errorStatusGaugeVec,
				durationGaugeVec)
			break
		case *network.EventLoadingFailed:
			m.EventLoadingFailed = ev.(*network.EventLoadingFailed)
			if cdpConfig.IgnoreMediaFile && m.EventLoadingFailed.Type == "Media" {
				break
			}
			m.WaitGroup.Add(1)
			go m.networkEventLoadingFailed(
				otherErrorGaugeVec,
				requestList[m.EventLoadingFailed.RequestID.String()])
			cancel()
			break
		case *network.EventRequestWillBeSent:
			m.WaitGroup.Add(1)
			go func(r *network.EventRequestWillBeSent) {
				requestList[r.RequestID.String()] = r.Request.URL
				httpRequestCountter.WithLabelValues(r.Request.Method).Add(1)
				defer m.WaitGroup.Done()
			}(ev.(*network.EventRequestWillBeSent))
			break
		case *page.EventDomContentEventFired:
			defer domTimer.ObserveDuration()
			break
		case *page.EventLoadEventFired:
			defer loadTimer.ObserveDuration()
			break
		case *cdplog.EventEntryAdded:
			m.CdpLog = ev.(*cdplog.EventEntryAdded)
			m.WaitGroup.Add(1)
			go m.cdplogEventEntryAdded(consoleMessageGaugeVec)
			break
		}
	})

	clearDOM := strings.TrimSuffix(string(cdpConfig.BodyWaitDOMLoad), "\n")
	clearDOM = strings.Trim(clearDOM, `'"`)

	err = chromedp.Run(ctx,
		network.Enable(),
		network.ClearBrowserCache(),
		page.Enable(),
		cdplog.Enable(),
		chromedp.Navigate(m.URL.String()),
		chromedp.WaitVisible(clearDOM, chromedp.ByQuery),
		chromedp.Sleep(cdpConfig.ChromedpWaitTime),
	)

	if err != nil {
		level.Error(logger).Log(m.URL.String(), err)
		return false
	}

	m.WaitGroup.Wait()

	success = true
	return
}

func (m *MonitorInfo) cdplogEventEntryAdded(consoleMessageGaugeVec *prometheus.GaugeVec) {
	defer m.WaitGroup.Done()

	clog := *m.CdpLog.Entry

	switch clog.Level {
	case "warning":
		level.Warn(*m.Logger).Log("Source", clog.Source, "Message", clog.Text, "URL", clog.URL)
	case "error":
		level.Error(*m.Logger).Log("Source", clog.Source, "Message", clog.Text, "URL", clog.URL)
	default:
		level.Info(*m.Logger).Log("Source", clog.Source, "Message", clog.Text, "URL", clog.URL)
	}
	consoleMessageGaugeVec.WithLabelValues(clog.Level.String(), clog.Source.String(), clog.Text, clog.URL).Set(1)
}

func (m *MonitorInfo) networkEventLoadingFailed(otherErrorGaugeVec *prometheus.GaugeVec, reqURL string) {
	defer m.WaitGroup.Done()

	level.Error(*m.Logger).Log(
		"Site", m.URL.String(),
		"RequestID", m.EventLoadingFailed.RequestID,
		"ErrorText", m.EventLoadingFailed.ErrorText,
		"Type", m.EventLoadingFailed.Type.String(),
		"BlockedReason", m.EventLoadingFailed.BlockedReason.String(),
		"ReqURL", reqURL)
	otherErrorGaugeVec.WithLabelValues(
		m.URL.String(),
		m.EventLoadingFailed.BlockedReason.String(),
		m.EventLoadingFailed.ErrorText,
		m.EventLoadingFailed.Type.String(),
		reqURL).Set(1)
}

func (m *MonitorInfo) networkEventResponseReceived(cancel context.CancelFunc, errorStatusGaugeVec, durationGaugeVec *prometheus.GaugeVec) {
	defer m.WaitGroup.Done()

	res := m.EventResponseReceived.Response
	if res.ConnectionID != 0 && res.RemoteIPAddress != "" {
		switch true {
		case (res.Status > 499):
			level.Error(*m.Logger).Log(
				"Target", m.URL.String(),
				"status", res.Status,
				"URL", res.URL,
				"ServerIP", res.RemoteIPAddress,
				"Latency", res.Timing.ReceiveHeadersEnd)
			errorStatusGaugeVec.WithLabelValues(res.URL, res.RemoteIPAddress, strconv.FormatInt(res.Status, 10)).Set(1)
			cancel()
			break
		case (res.Status < 200 || res.Status > 399):
			level.Warn(*m.Logger).Log(
				"Target", m.URL.String(),
				"status", res.Status,
				"URL", res.URL,
				"ServerIP", res.RemoteIPAddress,
				"Latency", res.Timing.ReceiveHeadersEnd)
			errorStatusGaugeVec.WithLabelValues(res.URL, res.RemoteIPAddress, strconv.FormatInt(res.Status, 10)).Set(1)
			break
		case (res.Timing.ReceiveHeadersEnd > m.ResponseLatencyMS):
			durationGaugeVec.WithLabelValues(
				res.URL,
				res.RemoteIPAddress,
				strconv.FormatInt(res.Status, 10)).Set(res.Timing.ReceiveHeadersEnd / 1000)
			break
		}
	}
}
