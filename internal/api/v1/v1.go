// Copyright 2024 Sudo Sweden AB
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"fmt"

	"net/http"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/sudoswedenab/dockyards-backend/api/apiutil"
	dyconfig "github.com/sudoswedenab/dockyards-backend/api/config"
	"github.com/sudoswedenab/dockyards-backend/api/jwt"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"github.com/sudoswedenab/dockyards-backend/api/console"
	authorizationv1 "k8s.io/api/authorization/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	k8ws "k8s.io/client-go/transport/websocket"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kubevirtv1 "kubevirt.io/client-go/kubevirt/typed/core/v1"
)

type API struct {
	client.Client
	consoleUpgrader   websocket.Upgrader
	vncUpgrader       websocket.Upgrader
	kubevirt          KubeVirt
	systemNamespace   string
	config            *dyconfig.ConfigManager
	subjectAllowCache SubjectAllowCache
	kvclient          kubevirtv1.KubevirtV1Interface
}

var _ jwt.RequestAuthenticator = &API{}

func (a *API) GetSystemNamespace() string {
	return a.systemNamespace
}

type Subject = string
type ResourceName = string

type AllowList struct {
	Mtx             sync.Mutex
	Subject         string
	VirtualMachines map[ResourceName]bool
	Events          chan console.Event
	ConnectionID    uint16
}

type SubjectAllowCache struct {
	StreamEvents bool
	Client       client.Client
	Clientset    *kubernetes.Clientset
	Mtx          sync.Mutex
	AllowList    map[*console.Stream]*AllowList
}

func (c *SubjectAllowCache) isAllowedVM(ctx context.Context, stream *console.Stream, subject string, resourceName string) bool {
	allowList := c.allowList(ctx, subject, resourceName, stream)

	allowList.Mtx.Lock()
	allowed, found := allowList.VirtualMachines[resourceName]
	if !found {
		allowed = c.isAllowedVMSlow(ctx, subject, resourceName)
		allowList.VirtualMachines[resourceName] = allowed
	}
	allowList.Mtx.Unlock()

	return allowed
}

func (c *SubjectAllowCache) allowList(ctx context.Context, subject string, resourceName string, stream *console.Stream) *AllowList {
	c.Mtx.Lock()
	if c.AllowList == nil {
		c.AllowList = make(map[*console.Stream]*AllowList)
	}
	allowList, found := c.AllowList[stream]
	if !found {
		allowList = &AllowList{
			Subject:         subject,
			VirtualMachines: map[ResourceName]bool{},
		}
		if c.StreamEvents {
			allowList.Events = make(chan console.Event, 1024)
		}
		c.AllowList[stream] = allowList
		go c.watchSubjectAllowEntries(ctx, allowList, c, stream)
	}
	c.Mtx.Unlock()
	return allowList
}

// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

func (c *SubjectAllowCache) isAllowedVMSlow(ctx context.Context, subject string, resourceName string) bool {
	logger := console.LoggerFrom(ctx)

	parts := strings.Split(resourceName, "/")
	if len(parts) != 2 {
		return false
	}
	namespace := parts[0]
	name := parts[1]

	resourceAttributes := authorizationv1.ResourceAttributes{
		Group:     dockyardsv1.GroupVersion.Group,
		Namespace: namespace,
		Name:      name,
		Resource:  dockyardsv1.ResourceVirtualMachineInstance.String(),
		Verb:      "update",
		Version:   dockyardsv1.GroupVersion.Version,
	}

	allowed, err := apiutil.IsSubjectAllowed(ctx, c.Client, subject, &resourceAttributes)
	if err != nil {
		logger.Error("could not check if subject is allowed", "err", err)
		return false
	}

	return allowed
}

func (c *SubjectAllowCache) watchSubjectAllowEntries(ctx context.Context, allowList *AllowList, allowCache *SubjectAllowCache, stream *console.Stream) {
	logger := console.LoggerFrom(ctx)

	defer func() {
		allowCache.Mtx.Lock()
		delete(allowCache.AllowList, stream)
		allowCache.Mtx.Unlock()
	}()

	tick := make(chan struct{})
	synced := atomic.Bool{}
	funcs := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if synced.Load() {
				tick <- struct{}{}
			}
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if synced.Load() {
				tick <- struct{}{}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if synced.Load() {
				tick <- struct{}{}
			}
		},
	}

	factory := informers.NewSharedInformerFactory(c.Clientset, 10*time.Minute)

	informers := []cache.SharedIndexInformer{
		factory.Rbac().V1().Roles().Informer(),
		factory.Rbac().V1().ClusterRoles().Informer(),
		factory.Rbac().V1().RoleBindings().Informer(),
		factory.Rbac().V1().ClusterRoleBindings().Informer(),
	}

	for _, informer := range informers {
		_, err := informer.AddEventHandlerWithOptions(funcs, cache.HandlerOptions{})
		if err != nil {
			logger.Error("could not add event handler to informer", "err", err)
			return
		}
	}
	for _, informer := range informers {
		go informer.RunWithContext(ctx)
	}

	for _, informer := range informers {
		select {
		case <-ctx.Done():
			return
		case <-informer.HasSyncedChecker().Done():
			continue
		}
	}
	synced.Store(true)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			allowList.Mtx.Lock()
			subject := allowList.Subject
			for resourceName := range allowList.VirtualMachines {
				oldValue := allowList.VirtualMachines[resourceName]
				newValue := c.isAllowedVMSlow(ctx, subject, resourceName)
				if oldValue != newValue {
					logger.Info("authorization changed", "subject", subject, "resourecName", resourceName, "oldValue", oldValue, "newValue", newValue)
					allowList.VirtualMachines[resourceName] = newValue
					if allowList.Events != nil {
						allowList.Events <- console.AuthorizationChangeEvent(stream, newValue)
					}
				}
			}
			allowList.Mtx.Unlock()
		}
	}
}

// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachines/start,verbs=update
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachines/stop,verbs=update
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachines/restart,verbs=update
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/softreboot,verbs=update
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/pause,verbs=update
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/unpause,verbs=update
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/console,verbs=get
// +kubebuilder:rbac:groups=subresources.kubevirt.io,resources=virtualmachineinstances/vnc,verbs=get

type KubeVirt struct {
	cfg *rest.Config
}

func (v *KubeVirt) connect(ctx context.Context, path string) (*websocket.Conn, error) {
	logger := console.LoggerFrom(ctx)

	rt, connHolder, err := k8ws.RoundTripperFor(v.cfg)
	if err != nil {
		return nil, err
	}

	targetURL, err := url.Parse(v.cfg.Host)
	if err != nil {
		return nil, err
	}

	targetURL.Path = path
	u := targetURL.String()
	logger.Debug("connecting", "url", u, "config", v.cfg)

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}

	return k8ws.Negotiate(rt, connHolder, req, "plain.kubevirt.io")
}

func (v *KubeVirt) SerialConsole(ctx context.Context, namespace string, name string) (*websocket.Conn, error) {
	if namespace == "" {
		namespace = "default"
	}
	return v.connect(ctx, fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s/console", namespace, name))
}

func (v *KubeVirt) VNC(ctx context.Context, namespace string, name string) (*websocket.Conn, error) {
	if namespace == "" {
		namespace = "default"
	}
	return v.connect(ctx, fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachineinstances/%s/vnc", namespace, name))
}

func NewAPI(mgr manager.Manager, configManager *dyconfig.ConfigManager, systemNamespace string) (*API, error) {
	const bufferSize = 10240
	pool := &sync.Pool{}

	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, err
	}

	kvclient, err := kubevirtv1.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, err
	}

	api := API{
		Client:          mgr.GetClient(),
		config:          configManager,
		systemNamespace: systemNamespace,
		kubevirt: KubeVirt{
			cfg: mgr.GetConfig(),
		},
		consoleUpgrader: websocket.Upgrader{
			Subprotocols: []string{
				"multiplexed.console.dockyards.io",
			},
			ReadBufferSize: bufferSize,
			WriteBufferSize: bufferSize,
			WriteBufferPool: pool,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return false // This request was not by a webbrowser
				}

				allowedOrigins := configManager.GetStringSliceOrDefault(dyconfig.KeyAllowedOrigins, nil)
				for _, o := range allowedOrigins {
					if o == origin {
						return true
					}
				}

				return false
			},
		},
		vncUpgrader: websocket.Upgrader{
			Subprotocols: []string{
				"plain.vnc.dockyards.io",
			},
			ReadBufferSize: bufferSize,
			WriteBufferSize: bufferSize,
			WriteBufferPool: pool,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return false // This request was not by a webbrowser
				}

				allowedOrigins := configManager.GetStringSliceOrDefault(dyconfig.KeyAllowedOrigins, nil)
				for _, o := range allowedOrigins {
					if o == origin {
						return true
					}
				}

				return false
			},
		},
		subjectAllowCache: SubjectAllowCache{
			StreamEvents: true,
			Client:       mgr.GetClient(),
			Clientset:    clientset,
		},
		kvclient: kvclient,
	}

	return &api, nil
}

func (a *API) RegisterRoutes(mux *http.ServeMux) {
	enabledWithAuth := func(next http.HandlerFunc) http.Handler {
		return checkEnabled(a.config, jwt.WithAuth(a, next))
	}

	mux.Handle("GET /v1/virtualmachines/console", enabledWithAuth(a.console))
	mux.Handle("GET /v1/virtualmachines/namespace/{namespace}/name/{name}/vnc", enabledWithAuth(a.vnc))
	mux.Handle("PUT /v1/virtualmachines/namespace/{namespace}/name/{name}/{powerstate}", enabledWithAuth(a.setVirtualMachineState))
}

type PowerState string

const (
	PowerStateStart      PowerState = "start"
	PowerStateStop       PowerState = "stop"
	PowerStateRestart    PowerState = "restart"
	PowerStateSoftReboot PowerState = "softreboot"
	PowerStatePause      PowerState = "pause"
	PowerStateUnpause    PowerState = "unpause"
)

func (a *API) setVirtualMachineState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := console.LoggerFrom(ctx)

	namespace := r.PathValue("namespace")
	if namespace == "" {
		namespace = "default"
	}
	name := r.PathValue("name")
	powerState := PowerState(r.PathValue("powerstate"))

	subject := jwt.SubjectFrom(ctx)

	if !a.isAllowedToChangeVMPowerState(ctx, subject, namespace, name) {
		w.WriteHeader(http.StatusForbidden)

		return
	}

	var err error
	switch powerState {
	case PowerStateStart:
		err = a.kvclient.VirtualMachines(namespace).Start(r.Context(), name, nil)
	case PowerStateStop:
		err = a.kvclient.VirtualMachines(namespace).Stop(r.Context(), name, nil)
	case PowerStateRestart:
		err = a.kvclient.VirtualMachines(namespace).Restart(r.Context(), name, nil)
	case PowerStateSoftReboot:
		err = a.kvclient.VirtualMachineInstances(namespace).SoftReboot(r.Context(), name)
	case PowerStatePause:
		err = a.kvclient.VirtualMachineInstances(namespace).Pause(r.Context(), name, nil)
	case PowerStateUnpause:
		err = a.kvclient.VirtualMachineInstances(namespace).Unpause(r.Context(), name, nil)
	default:
		logger.Error("invalid power state", "powerState", powerState)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid powerstate: " + powerState)) // Best effort

		return
	}

	if err != nil {
		logger.Error("could not set vm power state", "powerState", powerState, "err", err)

		var e *apierrors.StatusError
		if errors.As(err, &e) {
			code := int(e.Status().Code)
			if code == http.StatusConflict {
				// Pausing something that is already paused is fine.
				w.WriteHeader(http.StatusOK)

				return
			}
			if code == 0 {
				code = http.StatusInternalServerError
			}
			w.WriteHeader(code)
			_, _ = w.Write([]byte(e.Status().Message)) // Best effort

			return
		}

		w.WriteHeader(http.StatusInternalServerError)

		return
	}

}

func (a *API) console(w http.ResponseWriter, r *http.Request) {
	logger := console.LoggerFrom(r.Context())

	accessKey, err := jwt.GetAccessKey(r.Context(), a, a.systemNamespace)
	if err != nil {
		logger.Error("could not get access key", "err", err)
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	console.HandleRequest(w, r, console.RequestHandlerOptions{
		AccessKey: accessKey,
		Upgrader:  &a.consoleUpgrader,
		IsAllowed: a.isAllowedConsole,
		Start:     a.startConsole,
		Events:    make(chan console.Event, 1024),
	})
}

func (a *API) isAllowedConsole(ctx context.Context, server *console.Server, stream *console.Stream) bool {
	return a.subjectAllowCache.isAllowedVM(ctx, stream, server.Subject, stream.ResourceName)
}

func (a *API) isAllowedVNC(ctx context.Context, subject string, namespace string, name string) bool {
	return a.subjectAllowCache.isAllowedVM(ctx, nil, subject, fmt.Sprintf("%s/%s", namespace, name))
}

func (a *API) isAllowedToChangeVMPowerState(ctx context.Context, subject string, namespace string, name string) bool {
	return a.subjectAllowCache.isAllowedVM(ctx, nil, subject, fmt.Sprintf("%s/%s", namespace, name))
}

func (a *API) startConsole(ctx context.Context, server *console.Server, stream *console.Stream) error {
	ctx, cancel := context.WithCancel(ctx)
	stream.Cancel = cancel
	stream.Done = ctx.Done

	var err error

	objectName, err := cache.ParseObjectName(stream.ResourceName)
	if err != nil {
		return fmt.Errorf("could not parse resource name: %w", err)
	}
	if !a.isAllowedConsole(ctx, server, stream) {
		return apierrors.NewUnauthorized("subject is not allowed to open console")
	}

	logger := console.LoggerFrom(ctx)

	allowList := a.subjectAllowCache.allowList(ctx, server.Subject, stream.ResourceName, stream)

	kvconsole, err := a.kubevirt.SerialConsole(ctx, objectName.Namespace, objectName.Name)
	if err != nil {
		return fmt.Errorf("could not create serial console: %w", err)
	}

	logger.Debug("connecting", "namespace", objectName.Namespace, "name", objectName.Name)

	textOutput := make(chan []byte, 1024)
	go func() {
		defer close(textOutput)
		for {
			mt, data, err := kvconsole.ReadMessage()
			if err != nil {
				logger.Error("could not read tty", "err", err)
				stream.Close()
				cancel()
				return
			}
			if mt != websocket.BinaryMessage {
				logger.Error("message was not binary, ignoring it")
				continue
			}
			textOutput <- data
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-stream.Input:
				switch m.Kind {
				case console.ClientMessageKindInput:
					err := kvconsole.WriteMessage(websocket.BinaryMessage, m.Input.Data)
					if err != nil {
						logger.Error("could not write to pty", "err", err)
						return
					}
				case console.ClientMessageKindResize:
					logger.Warn("kubevirt does not implement console resizing")
				}
			case data := <-textOutput:
				server.Events <- console.InputEvent(stream, data)
			case event := <-allowList.Events:
				server.Events <- event
			}
		}
	}()

	return nil
}

func (a *API) vnc(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	logger := console.LoggerFrom(ctx)

	subject := jwt.SubjectFrom(ctx)

	namespace := r.PathValue("namespace")
	name := r.PathValue("name")

	logger.Debug("connecting VNC")

	if !a.isAllowedVNC(ctx, subject, namespace, name) {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	conn, err := a.vncUpgrader.Upgrade(w, r, nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		logger.Error("could not upgrade connection", "err", err)

		return
	}
	conn.SetCloseHandler(func(code int, text string) error {
		cancel()

		return nil
	})

	vnc, err := a.kubevirt.VNC(ctx, namespace, name)
	if err != nil {
		logger.Error("could not start vnc session", "err", err)

		return
	}
	vnc.SetCloseHandler(func(code int, text string) error {
		cancel()

		return nil
	})

	send := make(chan []byte)
	recv := make(chan []byte)
	go func() {
		for {
			select {
			case <-ctx.Done():
				cleanClose := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
				_ = vnc.WriteMessage(websocket.CloseMessage, cleanClose)
				_ = conn.WriteMessage(websocket.CloseMessage, cleanClose)
				return
			case data := <-send:
				err = conn.WriteMessage(websocket.BinaryMessage, data)
				if err != nil {
					logger.Error("could not write vnc message to websocket", "err", err)

					return
				}
			case data := <-recv:
				err = vnc.WriteMessage(websocket.BinaryMessage, data)
				if err != nil {
					logger.Error("could not write websocket message to vnc", "err", err)

					return
				}
			}
		}
	}()

	go func() {
		defer close(send)
		for {
			mt, data, err := vnc.ReadMessage()
			if err != nil {
				logger.Error("could not read vnc message", "err", err)
				cancel()

				return
			}
			if mt != websocket.BinaryMessage {
				logger.Error("websocket message was not binary")

				continue
			}

			if !a.isAllowedVNC(ctx, subject, namespace, name) {
				logger.Error("subject is not allowed vnc", "subject", subject, "namespace", namespace, "name", name)
				cancel()
				return
			}

			send <- data
		}
	}()

	defer close(recv)
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			logger.Error("could not read websocket message", "err", err)
			cancel()

			return
		}
		if mt != websocket.BinaryMessage {
			logger.Error("websocket message was not binary")

			continue
		}

		if !a.isAllowedVNC(ctx, subject, namespace, name) {
			logger.Error("subject is not allowed vnc", "subject", subject, "namespace", namespace, "name", name)
			cancel()
			return
		}

		recv <- data
	}
}

func checkEnabled(configManager *dyconfig.ConfigManager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if configManager.GetBool(dyconfig.KeyVirtualMachineAPI) {
			next.ServeHTTP(w, r)

			return
		}

		logger := console.LoggerFrom(r.Context())
		message := string(dyconfig.KeyVirtualMachineAPI) + " has not been set to true in dockyards config map"
		logger.Error(message)

		w.WriteHeader(http.StatusNotImplemented)
		_, _ = w.Write([]byte(message)) // Best effort
	})
}
