package server

import (
	"context"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// WakerFunc is a function that wakes up a server and returns its address.
type WakerFunc func(ctx context.Context) (string, error)

// SleeperFunc is a function that puts a server to sleep.
type SleeperFunc func(ctx context.Context) error

func buildWakerFromSleeper(endpoint string, sleeper SleeperFunc) WakerFunc {
	if sleeper == nil {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		if err := sleeper(ctx); err != nil {
			return "", err
		}
		return endpoint, nil
	}
}

var tcpShieldPattern = regexp.MustCompile("///.*")

// RouteFinder implementations find new routes in the system that can be tracked by a RoutesHandler
type RouteFinder interface {
	Start(ctx context.Context, handler RoutesHandler) error
	String() string
}

type RoutesHandler interface {
	CreateMapping(serverAddress string, backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string)
	SetDefaultRoute(backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string)
	// DeleteMapping requests that the serverAddress be removed from routes.
	// Returns true if the route existed.
	DeleteMapping(serverAddress string) bool
}

type IRoutes interface {
	RoutesHandler

	Reset()
	RegisterAll(mappings map[string]string)
	// FindBackendForServerAddress returns the host:port for the external server address, if registered.
	// Otherwise, an empty string is returned. Also returns the normalized version of the given serverAddress.
	// The 3rd value returned is the scalingTarget which indicates what endpoint to scale (may differ from backend when using proxy).
	// The 4th value returned is an (optional) "waker" function which a caller must invoke to wake up serverAddress.
	// The 5th value returned is an (optional) "sleeper" function which a caller must invoke to shut down serverAddress.
	HasRoute(serverAddress string) bool
	FindBackendForServerAddress(ctx context.Context, serverAddress string) (string, string, string, WakerFunc, SleeperFunc)
	GetSleepers(scalingTarget string) []SleeperFunc
	GetMappings() map[string]string
	GetDefaultRoute() (string, string, WakerFunc, SleeperFunc)
	SetFallbackRoute(backend string)
	GetFallbackRoute() string
	GetAsleepMOTD(serverAddress string) string
	GetLoadingMOTD(serverAddress string) string
	SimplifySRV(srvEnabled bool)
}

var Routes = NewRoutes()

func testBackendConnectivity(backend string) bool {
	if backend == "" {
		return false
	}

	conn, err := net.DialTimeout("tcp", backend, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	return true
}

func ProbeBackend(ctx context.Context, serverAddress string) (string, bool) {
	backend, _, _, _, _ := Routes.FindBackendForServerAddress(ctx, serverAddress)
	if backend == "" {
		return "", false
	}
	return backend, testBackendConnectivity(backend)
}

func NewRoutes() IRoutes {
	r := &routesImpl{
		mappings: make(map[string]mapping),
	}

	return r
}

func (r *routesImpl) RegisterAll(mappings map[string]string) {
	for k, v := range mappings {
		r.CreateMapping(k, v, "", nil, nil, "", "")
	}
}

type mapping struct {
	backend       string
	waker         WakerFunc
	sleeper       SleeperFunc
	asleepMOTD    string
	loadingMOTD   string
	scalingTarget string // The endpoint to scale (may differ from backend when using proxy)
}

type routesImpl struct {
	sync.RWMutex
	mappings      map[string]mapping
	defaultRoute  mapping
	fallbackRoute string
	simplifySRV   bool
}

func (r *routesImpl) Reset() {
	r.mappings = make(map[string]mapping)
	DownScaler.Reset()
}

func (r *routesImpl) SetDefaultRoute(backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	if scalingTarget == "" {
		scalingTarget = backend
	}
	r.defaultRoute = mapping{backend: backend, scalingTarget: scalingTarget, waker: waker, sleeper: sleeper, asleepMOTD: asleepMOTD, loadingMOTD: loadingMOTD}

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Using default route")
}

func (r *routesImpl) GetDefaultRoute() (string, string, WakerFunc, SleeperFunc) {
	return r.defaultRoute.backend, r.defaultRoute.scalingTarget, r.defaultRoute.waker, r.defaultRoute.sleeper
}

func (r *routesImpl) GetAsleepMOTD(serverAddress string) string {
	r.RLock()
	defer r.RUnlock()

	if serverAddress == "" {
		return r.defaultRoute.asleepMOTD
	}

	if m, ok := r.mappings[serverAddress]; ok {
		return m.asleepMOTD
	}
	return ""
}

func (r *routesImpl) GetLoadingMOTD(serverAddress string) string {
	r.RLock()
	defer r.RUnlock()

	if serverAddress == "" {
		return r.defaultRoute.loadingMOTD
	}

	if m, ok := r.mappings[serverAddress]; ok {
		return m.loadingMOTD
	}
	return ""
}

func (r *routesImpl) SetFallbackRoute(backend string) {
	r.fallbackRoute = backend

	logrus.WithFields(logrus.Fields{
		"backend": backend,
	}).Info("Using fallback route")
}

func (r *routesImpl) GetFallbackRoute() string {
	return r.fallbackRoute
}

func (r *routesImpl) SimplifySRV(srvEnabled bool) {
	r.simplifySRV = srvEnabled
}

func (r *routesImpl) HasRoute(serverAddress string) bool {
	r.RLock()
	defer r.RUnlock()

	_, exists := r.mappings[serverAddress]
	return exists
}

func (r *routesImpl) FindBackendForServerAddress(_ context.Context, serverAddress string) (string, string, string, WakerFunc, SleeperFunc) {
	r.RLock()
	defer r.RUnlock()

	// Trim off Forge null-delimited address parts like \x00FML3\x00
	serverAddress = strings.Split(serverAddress, "\x00")[0]

	// Trim off infinity-filter backslash address parts like \\GUID\\CLIENT_IP...
	serverAddress = strings.Split(serverAddress, "\\")[0]

	serverAddress = strings.ToLower(
		// trim the root zone indicator, see https://en.wikipedia.org/wiki/Fully_qualified_domain_name
		strings.TrimSuffix(serverAddress, "."))

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
	}).Debug("Finding backend for server address")

	if r.simplifySRV {
		parts := strings.Split(serverAddress, ".")
		tcpIndex := -1
		for i, part := range parts {
			if part == "_tcp" {
				tcpIndex = i
				break
			}
		}
		if tcpIndex != -1 {
			parts = parts[tcpIndex+1:]
		}

		serverAddress = strings.Join(parts, ".")
	}

	// Strip suffix of TCP Shield
	serverAddress = tcpShieldPattern.ReplaceAllString(serverAddress, "")

	backend := r.defaultRoute.backend
	scalingTarget := r.defaultRoute.scalingTarget
	waker := r.defaultRoute.waker
	sleeper := r.defaultRoute.sleeper

	if r.mappings != nil {
		if mapping, exists := r.mappings[serverAddress]; exists {
			backend = mapping.backend
			scalingTarget = mapping.scalingTarget
			waker = mapping.waker
			sleeper = mapping.sleeper
		}
	}

	if waker == nil && r.fallbackRoute != "" && !testBackendConnectivity(backend) {
		logrus.WithFields(logrus.Fields{
			"serverAddress": serverAddress,
			"backend":       backend,
		}).Warn("Mapped backend is unavailable, falling back to fallback route")
		backend = r.fallbackRoute
		scalingTarget = r.fallbackRoute
		waker = nil
		sleeper = nil
	}

	return backend, serverAddress, scalingTarget, waker, sleeper
}

func (r *routesImpl) GetSleepers(scalingTarget string) []SleeperFunc {
	r.RLock()
	defer r.RUnlock()

	var sleepers []SleeperFunc
	for _, m := range r.mappings {
		if m.scalingTarget == scalingTarget && m.sleeper != nil {
			sleepers = append(sleepers, m.sleeper)
		}
	}
	if r.defaultRoute.scalingTarget == scalingTarget && r.defaultRoute.sleeper != nil {
		sleepers = append(sleepers, r.defaultRoute.sleeper)
	}
	return sleepers
}

func (r *routesImpl) GetMappings() map[string]string {
	r.RLock()
	defer r.RUnlock()

	result := make(map[string]string, len(r.mappings))
	for k, v := range r.mappings {
		result[k] = v.backend
	}
	return result
}

func (r *routesImpl) DeleteMapping(serverAddress string) bool {
	r.Lock()
	defer r.Unlock()
	logrus.WithField("serverAddress", serverAddress).Info("Deleting route")

	if m, ok := r.mappings[serverAddress]; ok {
		DownScaler.Cancel(m.scalingTarget)
		delete(r.mappings, serverAddress)
		return true
	} else {
		return false
	}
}

func (r *routesImpl) CreateMapping(serverAddress string, backend string, scalingTarget string, waker WakerFunc, sleeper SleeperFunc, asleepMOTD string, loadingMOTD string) {
	r.Lock()
	defer r.Unlock()

	serverAddress = strings.ToLower(serverAddress)

	if scalingTarget == "" {
		scalingTarget = backend
	}

	logrus.WithFields(logrus.Fields{
		"serverAddress": serverAddress,
		"backend":       backend,
	}).Info("Created route mapping")
	r.mappings[serverAddress] = mapping{backend: backend, scalingTarget: scalingTarget, waker: waker, sleeper: sleeper, asleepMOTD: asleepMOTD, loadingMOTD: loadingMOTD}

	// Trigger auto scale down when mapping is created to ensure servers are shut down if router restarts
	if DownScaler != nil && scalingTarget != "" {
		DownScaler.Begin(scalingTarget)
	}
}
