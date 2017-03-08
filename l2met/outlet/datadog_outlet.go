// The outlet pkg is responsible for taking
// buckets from the reader, formatting them in the proper format
// and delivering the formatted datadog metrics to the metric API.
package outlet

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/winkapp/log-shuttle/l2met/bucket"
	"github.com/winkapp/log-shuttle/l2met/metchan"
	"github.com/winkapp/log-shuttle/l2met/metrics"
	"github.com/winkapp/log-shuttle/l2met/reader"
	"strings"
	"github.com/winkapp/log-shuttle"
)

type DataDogOutlet struct {
	inbox       chan *bucket.Bucket
	conversions chan *metrics.DataDog
	outbox      chan []*metrics.DataDog
	numOutlets  int
	rdr         *reader.Reader
	conn        *http.Client
	numRetries  int
	Mchan       *metchan.Channel
	verbose     bool
	quiet       bool
}

func buildDataDogClient(ttl time.Duration) *http.Client {
	tr := &http.Transport{
		DisableKeepAlives: false,
		Dial: func(n, a string) (net.Conn, error) {
			c, err := net.DialTimeout(n, a, ttl)
			if err != nil {
				return c, err
			}
			return c, c.SetDeadline(time.Now().Add(ttl))
		},
	}
	return &http.Client{Transport: tr}
}

func NewDataDogOutlet(cfg shuttle.Config, r *reader.Reader) *DataDogOutlet {
	l := &DataDogOutlet{
		conn:        buildDataDogClient(cfg.L2met_OutletTtl),
		inbox:       make(chan *bucket.Bucket, cfg.L2met_BufferSize),
		conversions: make(chan *metrics.DataDog, cfg.L2met_BufferSize),
		outbox:      make(chan []*metrics.DataDog, cfg.L2met_BufferSize),
		numOutlets:  cfg.L2met_Concurrency,
		numRetries:  cfg.L2met_OutletRetries,
		rdr:         r,
		verbose:     cfg.Verbose,
		quiet:       cfg.Quiet,
	}
	return l
}

func (l *DataDogOutlet) Start() {
	go l.rdr.Start(l.inbox)
	// Converting is CPU bound as it reads from memory
	// then computes statistical functions over an array.
	for i := 0; i < runtime.NumCPU(); i++ {
		go l.convert()
	}
	go l.groupByUser()
	for i := 0; i < l.numOutlets; i++ {
		go l.outlet()
	}
	go l.Report()
}

func (l *DataDogOutlet) convert() {
	for b := range l.inbox {
		var tags = strings.Split(b.Id.Tags, ",")
		for _, metric := range b.Metrics() {
			dd := metrics.DataDogConverter{Src: metric, Tags: tags,}
			for _, m := range dd.Convert() {
				l.conversions <- m
			}
		}
		delay := b.Id.Delay(time.Now())
		l.Mchan.Measure("outlet.delay", float64(delay))
	}
}

func (l *DataDogOutlet) groupByUser() {
	ticker := time.Tick(time.Millisecond * 200)
	m := make(map[string][]*metrics.DataDog)
	for {
		select {
		case <-ticker:
			for k, v := range m {
				if len(v) > 0 {
					l.outbox <- v
				}
				delete(m, k)
			}
		case payload := <-l.conversions:
			log.Printf("payload: %v\n", payload)
			usr := payload.Auth
			if _, present := m[usr]; !present {
				m[usr] = make([]*metrics.DataDog, 1, 300)
				m[usr][0] = payload
			} else {
				m[usr] = append(m[usr], payload)
			}
			if len(m[usr]) == cap(m[usr]) {
				l.outbox <- m[usr]
				delete(m, usr)
			}
		}
	}
}

func (l *DataDogOutlet) outlet() {
	for payloads := range l.outbox {
		if len(payloads) < 1 {
			if !l.quiet {
				log.Printf("at=%q\n", "empty-metrics-error")
			}
			continue
		}
		//Since a playload contains all metrics for
		//a unique datadog api_key, we can extract the user/pass
		//from any one of the payloads.
		api_key := payloads[0].Auth
		if l.verbose {
			for m := range payloads {
				log.Printf("---------------------- %v ----------------------\n", m)
				log.Printf("m.Metric:  %v\n", payloads[m].Metric)
				log.Printf("m.Host:    %v\n", payloads[m].Host)
				log.Printf("m.Tags:    %v\n", payloads[m].Tags)
				log.Printf("m.Type:    %v\n", payloads[m].Type)
				log.Printf("m.Auth:    %v\n", payloads[m].Auth)
				log.Printf("m.Points:  %v\n", payloads[m].Points)
				log.Println("------------------------------------------------")
			}
		}
		ddReq := &metrics.DataDogRequest{Series: payloads}
		j, err := json.Marshal(ddReq)
		if err != nil {
			if !l.quiet {
				log.Printf("at=json error=%s key=%s\n", err, api_key)
			}
			continue
		}

		if err := l.postWithRetry(api_key, j); err != nil {
			l.Mchan.Measure("outlet.drop", 1)
		}
	}
}

func (l *DataDogOutlet) postWithRetry(api_key string, body []byte) error {
	for i := 0; i <= l.numRetries; i++ {
		if err := l.post(api_key, body); err != nil {
			if !l.quiet {
				log.Printf("measure.datadog.error key=%s msg=%s attempt=%d\n", api_key, err, i)
			}
			if i == l.numRetries {
				return err
			}
			continue
		}
		return nil
	}
	//Should not be possible.
	return errors.New("Unable to post.")
}

func (l *DataDogOutlet) post(api_key string, body []byte) error {
	defer l.Mchan.Time("outlet.post", time.Now())
	if l.verbose {
		log.Printf("body: %s\n", string(body))
	}
	req, err := metrics.DataDogCreateRequest(metrics.DataDogUrl, api_key, body)
	resp, err := l.conn.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return metrics.DataDogHandleResponse(resp, body)
}

// Keep an eye on the lenghts of our buffers.
// If they are maxed out, something is going wrong.
func (l *DataDogOutlet) Report() {
	for range time.Tick(time.Second) {
		pre := "datadog-outlet."
		l.Mchan.Measure(pre+"inbox", float64(len(l.inbox)))
		l.Mchan.Measure(pre+"conversion", float64(len(l.conversions)))
		l.Mchan.Measure(pre+"outbox", float64(len(l.outbox)))
	}
}
