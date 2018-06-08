package naming_test

import (
	"fmt"
	"time"

	"github.com/Bilibili/discovery/naming"
)

// This Example register a server provider into discovery.
func ExampleDiscovery_Register() {
	conf := &naming.Config{
		Domain: "127.0.0.1:7171",
		Zone:   "sh1",
		Env:    "test",
	}
	dis := naming.New(conf)
	ins := &naming.Instance{
		Zone:  "sh1",
		Env:   "test",
		AppID: "provider",
		// Hostname:"", // NOTE: hostname 不需要，会优先使用discovery new时Config配置的值，如没有则从os.Hostname方法获取！！！
		Addrs:    []string{"http://172.0.0.1:8888", "grpc://172.0.0.1:9999"},
		Color:    "red",
		LastTs:   time.Now().Unix(),
		Metadata: map[string]string{"weight": "10"},
	}
	cancel, _ := dis.Register(ins)
	defer cancel() // NOTE: 注意一般在进程退出的时候执行，会调用discovery的cancel接口，使实例从discovery移除
	fmt.Println("register")
	// Unordered output4
}

type consumer struct {
	conf  *naming.Config
	appID string
	dis   *naming.Discovery
	ins   []*naming.Instance
}

// This Example show how get watch a server provier and get provider instances.
func ExampleDiscovery_Watch() {
	conf := &naming.Config{
		Domain: "127.0.0.1:7171",
		Zone:   "sh1",
		Env:    "test",
	}
	dis := naming.New(conf)
	c := &consumer{
		conf:  conf,
		appID: "provider",
		dis:   dis,
	}
	ch := dis.Watch(c.appID)
	go c.getInstances(ch)
	in := c.getInstance()
	_ = in
}

func (c *consumer) getInstances(ch <-chan struct{}) {
	for { // NOTE: 通过watch返回的event chan =>
		if _, ok := <-ch; !ok {
			return
		}
		// NOTE: <= 实时fetch最新的instance实例
		ins, ok := c.dis.Fetch(c.appID)
		if !ok {
			continue
		}
		// get local zone instances, otherwise get all zone instances.
		if in, ok := ins[c.conf.Zone]; ok {
			c.ins = in
		} else {
			for _, in := range ins {
				c.ins = append(c.ins, in...)
			}
		}
	}
}

func (c *consumer) getInstance() (ins *naming.Instance) {
	// get instance by loadbalance
	// you can use any loadbalance algorithm what you want.
	return
}
