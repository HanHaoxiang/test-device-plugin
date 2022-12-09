package server

import (
	"context"
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

const (
	resourceName		string = "resource/test"
	defaultTestLocation string = "/etc/tests"
	testSocket			string = "test.sock"
	// KubeletSocket kubelet	监听 unix 的名称
	KubeletSocket		string = "kubelet.sock"
	// DevicePluginPath	默认位置
	DevicePluginPath	string = "var/lib/kubelet/device-plugins/"
)

// TestServer 是一个 device plugin server
type TestServer struct {
	srv			*grpc.Server
	device		map[string]*pluginapi.Device
	notify		chan bool
	ctx			context.context
	cancel		context.CancelFunc
	restartFlag	bool	//本次是否是重启
}

// NewTestServer 实例化 testServer
func NewTestServer() *TestServer {
	ctx, cancel := context.WithCancenl(context.Background())
	return &TestServer{
		device:		make(map[string]*pluginapi.Device),
		srv:		grpc.NewServer(grpc.EmptyServerOption{}),
		notify:		make(chan bool),
		ctx:		ctx,
		cancel:		cancel,
		restartFlag:false,
	}
}

// Run 运行服务
func (s *TestServer) Run() error {
	// 发现本地设备
	err := s.listDevice()
	if err != nil {
		log.Fatalf("list device error: %v", err)
	}

	go func() {
		err := s.watchDevice()
		if err != nil {
			log.Println("watch devices error")
		}
	}()

	pluginapi.RegisterDevicePluginServer(s.srv, s)
	err = syscall.Unlink(DevicePluginPath + testSocket)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	l, err := net.Listen("unix", DevicePluginPath + testSocket)
	if err != nil {
		return err
	}

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			log.Printf("start GRPC server for '%s'", resourceName)
			err = s.srv.Serve(l)
			if err == nil {
				break
			}

			log.Printf("GRPC server for '%s' crashed with error: $v", resourceName, err)
			
			if restartCount > 5{
				log.Fatal("GRPC server for '%s' has repeatedly crashed recently. Quitting", resourceName)
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by lauching a blocking connection
	conn, err := s.dial(testServer, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// RegisterToKubelet 向kubelet注册device plugin
func (s *TestServer) RegisterToKubelet() error {
	socketFile := filepath.Join(DevicePluginPath + KubeletSocket)

	conn, err := s.dial(socketFile, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	req := &pluginapi.RegisterRequest{
		Version:		pluginapi.Version,
		Endpoint:		path.Base(DevicePluginPath + testSocket),
		resourceName:	resourceName,
	}
	log.Infof("Register to kubelet with endpoint %s", req.Endpoint)
	_, err = client.Register(context.Background(), req)
	if err != nil {
		return err
	}

	return nil
}

// GetDevicePluginOptions returns options to be communicated with Device
// Manager
func (s *TestServer) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	log.Infoln("GetDevicePluginOptions called")
	return &pluginapi.DevicePluginOptions{PreStartRequired: true}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list
func (s *TestServer) ListAndWatch(e *pluginapi.Empty, srv pluginapi.DevicePlugin_ListAndWatchServer) error {
	log.Infoln("ListAndWatch called")
	devs := make([]*pluginapi.Device, len(s.devices))

	i := 0
	for _, dev := range s.devices {
		devs[i] = dev
		i++
	}

	err := srv.Send(&pluginapi.ListAndWatchResponse{Device: devs})
	if err != nil {
		log.Errorf("ListAndWatch send device error: %v", err)
		return err
	}

	// 更新 device list
	for {
		log.Infoln("waiting for device change")
		select {
		case <-s.notify:
			log.Infoln("开始更新device list, 设备数:", len(s.devices))
			devs := make([]*pluginapi.Device, len(s.devices))

			i := 0
			for _, dev := range s.devices {
				devs[i] = dev
				i++
			}
			
			srv.Send(&pluginapi.ListAndWatchResponse{Device: devs})

		case <-s.ctx.Done():
			log.Info("ListAndWatch exit")
			return nil
		}
	}
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container
func (s *TestServer) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	log.Infoln("Allocate called")
	resps := &pluginapi.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
		log.Infof("received request: %v", strings.Join(req.DevicesIDs, ","))
		resp := pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{
				"TEST_DEVICES": strings.Join(req.DeviceIDs, ","),
			},
		}
		resps.ContainerResponses = append(resps.ContainerResponses, &resp)
	}
	return resps, nil
}

// PreStartContainer is called, if indicated by Device Plugin during registeration phase,
// before each container start. Device plugin can run device specific operations
// such as reseting the device before making devices available to the container
func (s *TestServer) PreStartContainer(ctx context.Context, req *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	log.Infoln("PreStartContainer called")
	return &pluginapi.PreStartContainerResponse{}, nil
}

// listDevice 从节点上发现设备
func (s *TestServer) listDevice() error {
	dir, err := ioutil.ReadDir(defaultTestLocation)
	if err != nil {
		return err
	}

	for _, f := range dir {
		if f.IsDir() {
			continue
		}

		sum := md5.Sum([]byte(f.Name()))
		s.devices[f.Name()] = &pluginapi.Device{
			ID:			string(sum[:]),
			Health:		pluginapi.Healthy,
		}
		log.Infof("find device '%s'", f.Name())
	}

	return nil
}

func (s *TestServer) watchDevice() error {
	log.Infoln("watching devices")
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("NewWatcher error:%v", err)
	}
	defer w.Close()

	done := make(chan bool)
	go func() {
		defer func() {
			done <- true
			log.Info("watch device exit")
		}()
		for {
			select {
			case event, ok := <-w.Events:
				if !ok{
					continue
				}
				log.Infoln("device event:", event.Op.String())

				if event.Op&fsnotify.Create == fsnotify.Create {
					//创建文件，增加 device
					sum := md5.Sum([]byte(event.Name))
					s.devices[event.Name] = &pluginapi.Device{
						ID:		string(sum[:]),
						Health: pluginapi.Healthy,
					}
					s.notify <- true
					log.Infoln("new device find:", event.Name)
				} else if event.Op&fsnotify.Remove == fsnotify.Remove {
					//删除文件，删除 device
					delete(s.devices, event.Name)
					s.notify <- true
					log.Infoln("device deleted:", evnet.Name)
				}

			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)

			case <-s.ctx.Done():
				break
			}
		}
	}()

	err = w.Add(defaultTestLocation)
	if err != nil {
		return fmt.Errorf("watch device error:%v", err)
	}
	<-done

	return nil
}

func (s *TestServer) dial(unixSockerPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSockerPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error){
			return net.DialTimeout("unix", addr, timeout)
		}),
	)
	
	if err != nil {
		return nil, err
	}
	
	return c, nil
}