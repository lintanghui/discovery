package naming

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ecode "github.com/Bilibili/discovery/errors"
	"github.com/Bilibili/discovery/lib/http"
	xtime "github.com/Bilibili/discovery/lib/time"
	log "github.com/golang/glog"
)

const (
	_registerURL = "http://%s/discovery/register"
	_cancelURL   = "http://%s/discovery/cancel"
	_renewURL    = "http://%s/discovery/renew"

	_pollURL  = "http://%s/discovery/polls"
	_nodesURL = "http://%s/discovery/nodes"

	_registerGap = 30 * time.Second

	_statusUP = "1"

	_errCodeOK = 0
	_errCodeNF = -404
)

var (
	_ Resolver = &Discovery{}
	_ Registry = &Discovery{}

	// ErrDuplication duplication treeid.
	ErrDuplication = errors.New("discovery: instance duplicate registration")
)

// Config discovery configures.
type Config struct {
	Domain string
	Zone   string
	Env    string
	Host   string
}

type appData struct {
	ZoneInstances map[string][]*Instance `json:"zone_instances"`
	LastTs        int64                  `json:"latest_timestamp"`
}

// Discovery is discovery client.
type Discovery struct {
	c          *Config
	once       sync.Once
	ctx        context.Context
	cancelFunc context.CancelFunc
	httpClient *http.Client

	mutex       sync.RWMutex
	apps        map[string]*appInfo
	registry    map[string]struct{}
	lastHost    string
	cancelPolls context.CancelFunc

	delete chan *appInfo
}

type appInfo struct {
	event   chan struct{}
	zoneIns atomic.Value
	lastTs  int64 // latest timestamp
}

func fixConfig(c *Config) {
	if c.Zone == "" {
		c.Zone = os.Getenv("ZONE")
	}
	if c.Env == "" {
		c.Env = os.Getenv("DEPLOY_ENV")
	}
	if c.Host == "" {
		c.Host, _ = os.Hostname()
	}
}

// New new a discovery client.
func New(c *Config) (d *Discovery) {
	fixConfig(c)
	ctx, cancel := context.WithCancel(context.Background())
	d = &Discovery{
		c:          c,
		ctx:        ctx,
		cancelFunc: cancel,
		apps:       map[string]*appInfo{},
		registry:   map[string]struct{}{},
		delete:     make(chan *appInfo, 10),
	}

	// httpClient
	cfg := &http.ClientConfig{
		Dial:      xtime.Duration(3 * time.Second),
		KeepAlive: xtime.Duration(40 * time.Second),
	}
	d.httpClient = http.NewClient(cfg)
	return
}

// Fetch returns the latest discovered instances by treeID
func (d *Discovery) Fetch(appID string) (ins map[string][]*Instance, ok bool) {
	d.mutex.RLock()
	app, ok := d.apps[appID]
	d.mutex.RUnlock()
	if ok {
		ins, ok = app.zoneIns.Load().(map[string][]*Instance)
	}
	return
}

// Unwatch unwatch app changes.
func (d *Discovery) Unwatch(appID string) {
	d.mutex.Lock()
	app, ok := d.apps[appID]
	if ok {
		delete(d.apps, appID)
	}
	d.mutex.Unlock()
	if ok {
		d.delete <- app
	}
}

// Watch watch the change of app instances by treeId  and return a chan to notify the instance change
func (d *Discovery) Watch(appID string) <-chan struct{} {
	d.mutex.RLock()
	app, ok := d.apps[appID]
	d.mutex.RUnlock()
	if !ok {
		app = &appInfo{event: make(chan struct{}, 1)}
		d.mutex.Lock()
		d.apps[appID] = app
		cancel := d.cancelPolls
		d.mutex.Unlock()
		log.Infof("disocvery: AddWatch(%s)", appID)
		if cancel != nil {
			cancel()
		}
	}
	d.once.Do(func() {
		go d.serverproc()
	})
	return app.event
}

// Reload reload the config
func (d *Discovery) Reload(c *Config) {
	fixConfig(c)
	d.mutex.Lock()
	d.c = c
	d.mutex.Unlock()
}

// Close stop all running process including discovery and register
func (d *Discovery) Close() error {
	d.cancelFunc()
	return nil
}

// Scheme return discovery's scheme
func (d *Discovery) Scheme() string {
	return "discovery"
}

// Register Register an instance with discovery and renew automatically
func (d *Discovery) Register(ins *Instance) (cancelFunc context.CancelFunc, err error) {
	d.mutex.Lock()
	if _, ok := d.registry[ins.AppID]; ok {
		err = ErrDuplication
	} else {
		d.registry[ins.AppID] = struct{}{}
	}
	d.mutex.Unlock()
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(d.ctx)
	if err = d.register(ctx, ins); err != nil {
		d.mutex.Lock()
		delete(d.registry, ins.AppID)
		d.mutex.Unlock()
		cancel()
		return
	}
	ch := make(chan struct{}, 1)
	cancelFunc = context.CancelFunc(func() {
		cancel()
		<-ch
	})
	go func() {
		ticker := time.NewTicker(_registerGap)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := d.renew(ctx, ins); err != nil && ecode.NothingFound.Equal(err) {
					d.register(ctx, ins)
				}
			case <-ctx.Done():
				d.cancel(ins)
				ch <- struct{}{}
				return
			}
		}
	}()
	return
}

// cancel Remove the registered instance from discovery
func (d *Discovery) cancel(ins *Instance) (err error) {
	d.mutex.RLock()
	c := d.c
	d.mutex.RUnlock()

	res := new(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	})
	uri := fmt.Sprintf(_cancelURL, c.Domain)
	params := d.newParams(c)
	params.Set("appid", ins.AppID)
	// request
	if err = d.httpClient.Post(context.TODO(), uri, "", params, &res); err != nil {
		log.Errorf("discovery cancel client.Get(%v) env(%s) appid(%s) hostname(%s) error(%v)",
			uri, c.Env, ins.AppID, c.Host, err)
		return
	}
	if ec := ecode.Int(res.Code); !ec.Equal(ecode.OK) {
		log.Warningf("discovery cancel client.Get(%v)  env(%s) appid(%s) hostname(%s) code(%v)",
			uri, c.Env, ins.AppID, c.Host, res.Code)
		err = ec
		return
	}
	log.Infof("discovery cancel client.Get(%v)  env(%s) appid(%s) hostname(%s) success",
		uri, c.Env, ins.AppID, c.Host)
	return
}

// register Register an instance with discovery
func (d *Discovery) register(ctx context.Context, ins *Instance) (err error) {
	d.mutex.RLock()
	c := d.c
	d.mutex.RUnlock()

	var metadata []byte
	if ins.Metadata != nil {
		if metadata, err = json.Marshal(ins.Metadata); err != nil {
			log.Errorf("discovery:register instance Marshal metadata(%v) failed!error(%v)", ins.Metadata, err)
		}
	}
	res := new(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	})
	uri := fmt.Sprintf(_registerURL, c.Domain)
	params := d.newParams(c)
	params.Set("appid", ins.AppID)
	params.Set("addrs", strings.Join(ins.Addrs, ","))
	params.Set("color", ins.Color)
	params.Set("version", ins.Version)
	params.Set("status", _statusUP)
	params.Set("metadata", string(metadata))
	if err = d.httpClient.Post(ctx, uri, "", params, &res); err != nil {
		log.Errorf("discovery: register client.Get(%v)  zone(%s) env(%s) appid(%s) addrs(%v) color(%s) error(%v)",
			uri, c.Zone, c.Env, ins.AppID, ins.Addrs, ins.Color, err)
		return
	}
	if ec := ecode.Int(res.Code); !ec.Equal(ecode.OK) {
		log.Warningf("discovery: register client.Get(%v)  env(%s) appid(%s) addrs(%v) color(%s)  code(%v)",
			uri, c.Env, ins.AppID, ins.Addrs, ins.Color, res.Code)
		err = ec
		return
	}
	log.Infof("discovery: register client.Get(%v) env(%s) appid(%s) addrs(%s) color(%s) success",
		uri, c.Env, ins.AppID, ins.Addrs, ins.Color)
	return
}

// renew Renew an instance with discovery
func (d *Discovery) renew(ctx context.Context, ins *Instance) (err error) {
	d.mutex.RLock()
	c := d.c
	d.mutex.RUnlock()

	res := new(struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	})
	uri := fmt.Sprintf(_renewURL, c.Domain)
	params := d.newParams(c)
	params.Set("appid", ins.AppID)
	if err = d.httpClient.Post(ctx, uri, "", params, &res); err != nil {
		log.Errorf("discovery: renew client.Get(%v)  env(%s) appid(%s) hostname(%s) error(%v)",
			uri, c.Env, ins.AppID, c.Host, err)
		return
	}
	if ec := ecode.Int(res.Code); !ec.Equal(ecode.OK) {
		err = ec
		if ec.Equal(ecode.NothingFound) {
			return
		}
		log.Errorf("discovery: renew client.Get(%v) env(%s) appid(%s) hostname(%s) code(%v)",
			uri, c.Env, ins.AppID, c.Host, res.Code)
		return
	}
	return
}

func (d *Discovery) serverproc() {
	var (
		retry  int
		update bool
		nodes  []string
		idx    uint64
		ctx    context.Context
		cancel context.CancelFunc
	)
	ticker := time.NewTicker(time.Minute * 30)
	defer ticker.Stop()
	for {
		if ctx == nil {
			ctx, cancel = context.WithCancel(d.ctx)
			d.mutex.Lock()
			d.cancelPolls = cancel
			d.mutex.Unlock()
		}
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			update = true
		case app := <-d.delete:
			close(app.event)
		default:
		}
		if len(nodes) == 0 || update {
			update = false
			tnodes := d.nodes()
			if len(tnodes) == 0 {
				time.Sleep(time.Second)
				retry++
				continue
			}
			retry = 0
			nodes = tnodes
			// FIXME: we should use rand.Shuffle() in golang 1.10
			shuffle(len(nodes), func(i, j int) {
				nodes[i], nodes[j] = nodes[j], nodes[i]
			})
		}
		apps, err := d.polls(ctx, nodes[int(idx%uint64(len(nodes)))])
		if err != nil {
			if ctx.Err() == context.Canceled {
				ctx = nil
				continue
			}
			idx++
			time.Sleep(time.Second)
			retry++
			continue
		}
		retry = 0
		d.broadcast(apps)
	}
}

func (d *Discovery) nodes() (nodes []string) {
	d.mutex.RLock()
	c := d.c
	d.mutex.RUnlock()

	res := new(struct {
		Code int `json:"code"`
		Data []struct {
			Addr string `json:"addr"`
		} `json:"data"`
	})
	uri := fmt.Sprintf(_nodesURL, c.Domain)
	if err := d.httpClient.Get(d.ctx, uri, "", nil, res); err != nil {
		log.Errorf("discovery: consumer client.Get(%v)error(%+v)", uri, err)
		return
	}
	if ec := ecode.Int(res.Code); !ec.Equal(ecode.OK) {
		log.Errorf("discovery: consumer client.Get(%v) error(%v)", uri, res.Code)
		return
	}
	if len(res.Data) == 0 {
		log.Warningf("discovery: get nodes(%s) failed,no nodes found!", uri)
		return
	}
	nodes = make([]string, 0, len(res.Data))
	for i := range res.Data {
		nodes = append(nodes, res.Data[i].Addr)
	}
	return
}

func (d *Discovery) polls(ctx context.Context, host string) (apps map[string]appData, err error) {
	var (
		lastTss []int64
		appIDs  []string
		changed bool
	)
	if host != d.lastHost {
		d.lastHost = host
		changed = true
	}
	d.mutex.RLock()
	c := d.c
	for k, v := range d.apps {
		if changed {
			v.lastTs = 0
		}
		appIDs = append(appIDs, k)
		lastTss = append(lastTss, v.lastTs)
	}
	d.mutex.RUnlock()
	if len(appIDs) == 0 {
		return
	}
	uri := fmt.Sprintf(_pollURL, host)
	res := new(struct {
		Code int                `json:"code"`
		Data map[string]appData `json:"data"`
	})
	params := url.Values{}
	params.Set("env", c.Env)
	params.Set("hostname", c.Host)
	for _, appid := range appIDs {
		params.Add("appid", appid)
	}
	for _, ts := range lastTss {
		params.Add("latest_timestamp", strconv.FormatInt(ts, 10))
	}
	if err = d.httpClient.Get(ctx, uri, "", params, res); err != nil {
		log.Errorf("discovery: client.Get(%s) error(%+v)", uri+"?"+params.Encode(), err)
		return
	}
	if ec := ecode.Int(res.Code); !ec.Equal(ecode.OK) {
		if !ec.Equal(ecode.NotModified) {
			log.Errorf("discovery: client.Get(%s) get error code(%d)", uri+"?"+params.Encode(), res.Code)
			err = ec
		}
		return
	}
	info, _ := json.Marshal(res.Data)
	for _, app := range res.Data {
		if app.LastTs == 0 {
			err = ecode.ServerErr
			log.Errorf("discovery: client.Get(%s) latest_timestamp is 0,instances:(%s)", uri+"?"+params.Encode(), info)
			return
		}
	}
	log.Infof("discovery: successfully polls(%s) instances (%s)", uri+"?"+params.Encode(), info)
	apps = res.Data
	return
}

func (d *Discovery) broadcast(apps map[string]appData) {
	for appID, v := range apps {
		var count int
		for zone, ins := range v.ZoneInstances {
			if len(ins) == 0 {
				delete(v.ZoneInstances, zone)
			}
			count += len(ins)
		}
		if count == 0 {
			continue
		}
		d.mutex.RLock()
		app, ok := d.apps[appID]
		d.mutex.RUnlock()
		if ok {
			app.lastTs = v.LastTs
			app.zoneIns.Store(v.ZoneInstances)
			select {
			case app.event <- struct{}{}:
			default:
			}
		}
	}
}

func (d *Discovery) newParams(c *Config) url.Values {
	params := url.Values{}
	params.Set("zone", c.Zone)
	params.Set("env", c.Env)
	params.Set("hostname", c.Host)
	return params
}

var r = rand.New(rand.NewSource(time.Now().UnixNano()))

// shuffle pseudo-randomizes the order of elements.
// n is the number of elements. Shuffle panics if n < 0.
// swap swaps the elements with indexes i and j.
func shuffle(n int, swap func(i, j int)) {
	if n < 0 {
		panic("invalid argument to Shuffle")
	}

	// Fisher-Yates shuffle: https://en.wikipedia.org/wiki/Fisher%E2%80%93Yates_shuffle
	// Shuffle really ought not be called with n that doesn't fit in 32 bits.
	// Not only will it take a very long time, but with 2³¹! possible permutations,
	// there's no way that any PRNG can have a big enough internal state to
	// generate even a minuscule percentage of the possible permutations.
	// Nevertheless, the right API signature accepts an int n, so handle it as best we can.
	i := n - 1
	for ; i > 1<<31-1-1; i-- {
		j := int(r.Int63n(int64(i + 1)))
		swap(i, j)
	}
	for ; i > 0; i-- {
		j := int(r.Int31n(int32(i + 1)))
		swap(i, j)
	}
}
