// An internal metrics channel.
// l2met internal components can publish their metrics
// here and they will be outletted to Librato.
package metchan

import (
	"strings"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ryandotsmith/l2met/bucket"
	"github.com/ryandotsmith/l2met/conf"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// Convert l2met data into Librato's API format.
type libratoMetric struct {
	Name   string  `json:"name"`
	Time   int64   `json:"measure_time"`
	Source string  `json:"source"`
	Count  int     `json:"count"`
	Sum    float64 `json:"sum"`
	Max    float64 `json:"max"`
	Min    float64 `json:"min"`
}

func (l *libratoMetric) String() string {
	layout := "source=%s "
	layout += "sample#%s.count=%d "
	layout += "sample#%s.sum=%f "
	layout += "sample#%s.max=%f "
	layout += "sample#%s.min=%f"
	return fmt.Sprintf(layout,
		l.Source,
		l.Name, l.Count,
		l.Name, l.Sum,
		l.Name, l.Max,
		l.Name, l.Min)
}

type libratoGauge struct {
	Gauges []*libratoMetric `json:"gauges"`
}

type Channel struct {
	// The time by which metchan will aggregate internal metrics.
	FlushInterval time.Duration
	// The Channel is thread-safe.
	sync.Mutex
	username string
	password string
	verbose  bool
	Enabled  bool
	Buffer   map[string]*bucket.Bucket
	outbox   chan *libratoMetric
	url      *url.URL
	source   string
	appName  string
}

// Returns an initialized Metchan Channel.
// Creates a new HTTP client for direct access to Librato.
// This channel is orthogonal with other librato http clients in l2met.
// If a blank URL is given, no metric posting attempt will be made.
// If verbose is set to true, the metric will be printed to STDOUT
// regardless of whether the metric is sent to Librato.
func New(cfg *conf.D) *Channel {
	c := new(Channel)

	// If the url is nil, then it wasn't initialized
	// by the conf pkg. If it is not nil, we will
	// enable the Metchan.
	if cfg.MetchanUrl != nil {
		c.url = cfg.MetchanUrl
		c.username = cfg.MetchanUrl.User.Username()
		c.password, _ = cfg.MetchanUrl.User.Password()
		c.url.User = nil
		c.Enabled = true
	}

	// This will enable writting to a logger.
	c.verbose = cfg.Verbose

	// Internal Datastructures.
	c.Buffer = make(map[string]*bucket.Bucket)
	c.outbox = make(chan *libratoMetric, 10)

	// Default flush interval.
	c.FlushInterval = time.Minute

	host, err := os.Hostname()
	if err == nil {
		c.source = host
	}
	c.appName = cfg.AppName
	return c
}

func (c *Channel) Start() {
	if c.Enabled {
		go c.scheduleFlush()
		go c.outlet()
	}
}

// Provide the time at which you started your measurement.
// Places the measurement in a buffer to be aggregated and
// eventually flushed to Librato.
func (c *Channel) Time(name string, t time.Time) {
	elapsed := time.Since(t) / time.Millisecond
	c.Measure(name, float64(elapsed))
}

func (c *Channel) Measure(name string, v float64) {
	if c.verbose {
		fmt.Printf("source=%s measure#%s=%f\n", c.source, name, v)
	}
	if !c.Enabled {
		return
	}
	id := &bucket.Id{
		Resolution: c.FlushInterval,
		Name:       c.appName + "." + name,
		Units:      "ms",
		Source:     c.source,
	}
	c.add(id, v)
}

func (c *Channel) CountReq(user string) {
	usr := strings.Replace(user, "@", "_at_", -1)
	id := &bucket.Id{
		Resolution: c.FlushInterval,
		Name:       c.appName + "." + "receiver.requests",
		Units:      "requests",
		Source:     usr,
	}
	c.add(id, 1.0)
}

func (c *Channel) add(id *bucket.Id, val float64) {
	c.Lock()
	defer c.Unlock()
	key := id.Name + ":" + id.Source
	b, ok := c.Buffer[key]
	if !ok {
		b = &bucket.Bucket{Id: id}
		b.Vals = make([]float64, 1, 10000)
		c.Buffer[key] = b
	}
	// Instead of creating a new bucket struct with a new Vals slice
	// We will re-use the old bucket and reset the slice. This
	// dramatically decreases the amount of arrays created and thus
	// led to better memory utilization.
	latest := time.Now().Truncate(c.FlushInterval)
	if b.Id.Time != latest {
		b.Id.Time = latest
		b.Vals = b.Vals[:0]
	}
	b.Vals = append(b.Vals, val)
}

func (c *Channel) scheduleFlush() {
	for _ = range time.Tick(c.FlushInterval) {
		c.flush()
	}
}

func (c *Channel) flush() {
	c.Lock()
	defer c.Unlock()
	for _, b := range c.Buffer {
		c.outbox <- &libratoMetric{
			Name:   b.Id.Name,
			Time:   b.Id.Time.Unix(),
			Source: b.Id.Source,
			Count:  b.Count(),
			Sum:    b.Sum(),
			Max:    b.Max(),
			Min:    b.Min(),
		}
	}
}

func (c *Channel) outlet() {
	for met := range c.outbox {
		fmt.Printf("at=outlet-metric %s\n", met.String())
		if err := c.post(met); err != nil {
			fmt.Printf("at=metchan-post error=%s\n", err)
		}
	}
}

func (c *Channel) post(m *libratoMetric) error {
	p := &libratoGauge{[]*libratoMetric{m}}
	j, err := json.Marshal(p)
	if err != nil {
		return err
	}
	body := bytes.NewBuffer(j)
	req, err := http.NewRequest("POST", c.url.String(), body)
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("User-Agent", "l2met-metchan/0")
	req.SetBasicAuth(c.username, c.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var m string
		s, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			m = fmt.Sprintf("code=%d", resp.StatusCode)
		} else {
			m = fmt.Sprintf("code=%d resp=body=%s req-body=%s",
				resp.StatusCode, s, body)
		}
		return errors.New(m)
	}
	return nil
}
