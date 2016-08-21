package etcdv3

import (
	"crypto/tls"
	"crypto/x509"
	client "github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/yunify/metadata-proxy/log"
	"github.com/yunify/metadata-proxy/store"
	"github.com/yunify/metadata-proxy/util"
	"golang.org/x/net/context"
	"io/ioutil"
	"reflect"
	"time"
)

const SELF_MAPPING_PATH = "/_metadata-proxy/mapping"

// Client is a wrapper around the etcd client
type Client struct {
	client *client.Client
	prefix string
}

// NewEtcdClient returns an *etcd.Client with a connection to named machines.
func NewEtcdClient(prefix string, machines []string, cert, key, caCert string, basicAuth bool, username string, password string) (*Client, error) {
	var c *client.Client
	var err error

	tlsConfig := &tls.Config{
		InsecureSkipVerify: false,
	}

	cfg := client.Config{
		Endpoints:   machines,
		DialTimeout: time.Duration(3) * time.Second,
	}

	if basicAuth {
		cfg.Username = username
		cfg.Password = password
	}

	if caCert != "" {
		certBytes, err := ioutil.ReadFile(caCert)
		if err != nil {
			return nil, err
		}

		caCertPool := x509.NewCertPool()
		ok := caCertPool.AppendCertsFromPEM(certBytes)

		if ok {
			tlsConfig.RootCAs = caCertPool
		}
	}

	if cert != "" && key != "" {
		tlsCert, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []tls.Certificate{tlsCert}
	}

	cfg.TLS = tlsConfig

	c, err = client.New(cfg)
	if err != nil {
		return nil, err
	}

	return &Client{c, prefix}, nil
}

// GetValues queries etcd for key Recursive:true.
func (c *Client) GetValues(key string) (map[string]string, error) {
	return c.internalGetValues(c.prefix, key)
}

func (c *Client) internalGetValues(prefix, key string) (map[string]string, error) {
	vars := make(map[string]string)
	resp, err := c.client.Get(context.Background(), util.AppendPathPrefix(key, prefix), client.WithPrefix())
	if err != nil {
		return nil, err
	}

	err = handleGetResp(prefix, resp, vars)
	if err != nil {
		return vars, err
	}
	return vars, nil
}

// nodeWalk recursively descends nodes, updating vars.
func handleGetResp(prefix string, resp *client.GetResponse, vars map[string]string) error {
	if resp != nil {
		kvs := resp.Kvs
		for _, kv := range kvs {
			vars[string(kv.Key)] = string(kv.Value)
		}
		//TODO handle resp.More
	}
	return nil
}

func (c *Client) internalSync(prefix string, store store.Store, stopChan chan bool) {
	var rev int64 = 0
	inited := false
	for {
		ctx, cancel := context.WithCancel(context.Background())
		watchChan := c.client.Watch(ctx, prefix, client.WithPrefix(), client.WithRev(rev))

		cancelRoutine := make(chan bool)
		defer close(cancelRoutine)

		go func() {
			select {
			case <-stopChan:
				cancel()
			case <-cancelRoutine:
				return
			}
		}()

		for !inited {
			val, err := c.internalGetValues(prefix, "/")
			if err != nil {
				log.Error("GetValue from etcd key:%s, error-type: %s, error: %s", prefix, reflect.TypeOf(err), err.Error())
				switch err {
				case context.Canceled:
					log.Fatal("ctx is canceled by another routine: %v", err)
				case context.DeadlineExceeded:
					log.Fatal("ctx is attached with a deadline is exceeded: %v", err)
				//case rpctypes.ErrEmptyKey:
				//	log.Fatal("client-side error: %v", err)
				//	resp, createErr := c.client.Put(context.Background(), prefix, "")
				//	if createErr != nil {
				//		log.Error("Create dir %s error: %s", prefix, createErr.Error())
				//	}else{
				//		log.Info("Create dir %s resp: %v", prefix, resp)
				//	}
				default:
					log.Fatal("bad cluster endpoints, which are not etcd servers: %v", err)
				}
				time.Sleep(time.Duration(1000) * time.Millisecond)
				continue
			}
			store.SetBulk(val)
			inited = true
		}
		for resp := range watchChan {
			processSyncChange(prefix, store, &resp)
			rev = resp.Header.Revision
		}
	}
}

func processSyncChange(prefix string, store store.Store, resp *client.WatchResponse) {
	for _, event := range resp.Events {
		key := util.TrimPathPrefix(string(event.Kv.Key), prefix)
		value := string(event.Kv.Value)
		log.Debug("process sync change, event_type: %s, prefix: %v, key:%v, value: %v ", event.Type, prefix, key, value)
		switch event.Type {
		case mvccpb.PUT:
			store.Set(key, false, value)
		case mvccpb.DELETE:
			store.Delete(key)
		default:
			log.Warning("Unknow watch event type: %s ", event.Type)
			store.Set(key, false, value)

		}
	}
}

func (c *Client) Sync(store store.Store, stopChan chan bool) {
	go c.internalSync(c.prefix, store, stopChan)
}

func (c *Client) SetValues(values map[string]string) error {
	return c.internalSetValue(c.prefix, values)
}

func (c *Client) internalSetValue(prefix string, values map[string]string) error {
	for k, v := range values {
		k = util.AppendPathPrefix(k, prefix)
		log.Debug("SetValue prefix:%s, key:%s, value:%s", prefix, k, v)
		_, err := c.client.Put(context.Background(), k, v)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) Delete(key string) error {
	return c.internalDelete(c.prefix, key)
}

func (c *Client) internalDelete(prefix, key string) error {
	key = util.AppendPathPrefix(key, prefix)
	log.Debug("Delete from backend, key:%s", key)
	_, err := c.client.Delete(context.Background(), key, client.WithPrefix())
	return err
}

func (c *Client) SyncSelfMapping(mapping store.Store, stopChan chan bool) {
	go c.internalSync(SELF_MAPPING_PATH, mapping, stopChan)
}

func (c *Client) RegisterSelfMapping(clientIP string, mapping map[string]string) error {
	prefix := util.AppendPathPrefix(clientIP, SELF_MAPPING_PATH)
	oldMapping, _ := c.internalGetValues(prefix, "/")
	if oldMapping != nil {
		for k, _ := range oldMapping {
			if _, ok := mapping[k]; !ok {
				c.internalDelete("", k)
			}
		}
	}
	return c.internalSetValue(prefix, mapping)
}

func (c *Client) UnregisterSelfMapping(clientIP string) error {
	return c.internalDelete(SELF_MAPPING_PATH, clientIP)
}