package promhttp

import (
	"fmt"
	"io"
	"net/http"

	knot "github.com/eaciit/knot/knot.v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

var opts = HandlerOpts{}
var regGatherer prometheus.Gatherer
var regRegisterer prometheus.Registerer
var inFlightSem chan struct{}

func KnotHandler(wc *knot.WebContext) interface{} {
	regGatherer = prometheus.DefaultGatherer
	regRegisterer = prometheus.DefaultRegisterer

	if opts.MaxRequestsInFlight > 0 {
		inFlightSem = make(chan struct{}, opts.MaxRequestsInFlight)
	}

	// TODO not working with KNOT! fvck!
	cnt := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "promhttp_metric_handler_requests_total",
			Help: "Total number of scrapes by HTTP status code.",
		},
		[]string{"code"},
	)
	// Initialize the most likely HTTP status codes.
	cnt.WithLabelValues("200")
	cnt.WithLabelValues("500")
	cnt.WithLabelValues("503")
	if err := regRegisterer.Register(cnt); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			cnt = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	}

	gge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "promhttp_metric_handler_requests_in_flight",
		Help: "Current number of scrapes being served.",
	})
	if err := regRegisterer.Register(gge); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			gge = are.ExistingCollector.(prometheus.Gauge)
		} else {
			panic(err)
		}
	}

	return metrics(wc, cnt, gge)
}

func metrics(wc *knot.WebContext, counter *prometheus.CounterVec, g prometheus.Gauge) interface{} {
	wc.Config.NoLog = true
	w := wc.Writer
	req := wc.Request

	// con-currency right now handling request for metrics
	g.Inc()
	defer g.Dec()
	{
		code, method := checkLabels(counter)
		if code {
			counter.With(labels(code, method, req.Method, 0)).Inc()
		} else {
			counter.With(labels(code, method, req.Method, 0)).Inc()
		}
	}
	{

		if inFlightSem != nil {
			select {
			case inFlightSem <- struct{}{}: // All good, carry on.
				defer func() { <-inFlightSem }()
			default:
				http.Error(w, fmt.Sprintf(
					"Limit of concurrent requests reached (%d), try again later.", opts.MaxRequestsInFlight,
				), http.StatusServiceUnavailable)
				return nil
			}
		}

		mfs, err := regGatherer.Gather()
		if err != nil {
			if opts.ErrorLog != nil {
				opts.ErrorLog.Println("error gathering metrics:", err)
			}
			switch opts.ErrorHandling {
			case PanicOnError:
				panic(err)
			case ContinueOnError:
				if len(mfs) == 0 {
					http.Error(w, "No metrics gathered, last error:\n\n"+err.Error(), http.StatusInternalServerError)
					return nil
				}
			case HTTPErrorOnError:
				http.Error(w, "An error has occurred during metrics gathering:\n\n"+err.Error(), http.StatusInternalServerError)
				return nil
			}
		}

		contentType := expfmt.Negotiate(req.Header)
		buf := getBuf()
		defer giveBuf(buf)
		writer, encoding := decorateWriter(req, buf, opts.DisableCompression)
		enc := expfmt.NewEncoder(writer, contentType)
		var lastErr error
		for _, mf := range mfs {
			if err := enc.Encode(mf); err != nil {
				lastErr = err
				if opts.ErrorLog != nil {
					opts.ErrorLog.Println("error encoding metric family:", err)
				}
				switch opts.ErrorHandling {
				case PanicOnError:
					panic(err)
				case ContinueOnError:
					// Handled later.
				case HTTPErrorOnError:
					http.Error(w, "An error has occurred during metrics encoding:\n\n"+err.Error(), http.StatusInternalServerError)
					return nil
				}
			}
		}
		if closer, ok := writer.(io.Closer); ok {
			closer.Close()
		}
		if lastErr != nil && buf.Len() == 0 {
			http.Error(w, "No metrics encoded, last error:\n\n"+lastErr.Error(), http.StatusInternalServerError)
			return nil
		}
		header := w.Header()
		header.Set(contentTypeHeader, string(contentType))
		header.Set(contentLengthHeader, fmt.Sprint(buf.Len()))
		if encoding != "" {
			header.Set(contentEncodingHeader, encoding)
		}
		if _, err := w.Write(buf.Bytes()); err != nil && opts.ErrorLog != nil {
			opts.ErrorLog.Println("error while sending encoded metrics:", err)
		}
		// TODO(beorn7): Consider streaming serving of metrics.
	}
	return nil
}
