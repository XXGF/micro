// Package api is an API Gateway
package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-acme/lego/v3/providers/dns/cloudflare"
	"github.com/gorilla/mux"
	"github.com/micro/cli/v2"
	"github.com/micro/go-micro/v2"
	ahandler "github.com/micro/go-micro/v2/api/handler"
	aapi "github.com/micro/go-micro/v2/api/handler/api"
	"github.com/micro/go-micro/v2/api/handler/event"
	ahttp "github.com/micro/go-micro/v2/api/handler/http"
	arpc "github.com/micro/go-micro/v2/api/handler/rpc"
	"github.com/micro/go-micro/v2/api/handler/web"
	"github.com/micro/go-micro/v2/api/resolver"
	"github.com/micro/go-micro/v2/api/resolver/grpc"
	"github.com/micro/go-micro/v2/api/resolver/host"
	"github.com/micro/go-micro/v2/api/resolver/path"
	"github.com/micro/go-micro/v2/api/router"
	regRouter "github.com/micro/go-micro/v2/api/router/registry"
	"github.com/micro/go-micro/v2/api/server"
	"github.com/micro/go-micro/v2/api/server/acme"
	"github.com/micro/go-micro/v2/api/server/acme/autocert"
	"github.com/micro/go-micro/v2/api/server/acme/certmagic"
	httpapi "github.com/micro/go-micro/v2/api/server/http"
	log "github.com/micro/go-micro/v2/logger"
	"github.com/micro/go-micro/v2/sync/memory"
	"github.com/micro/micro/v2/api/auth"
	"github.com/micro/micro/v2/internal/handler"
	"github.com/micro/micro/v2/internal/helper"
	"github.com/micro/micro/v2/internal/namespace"
	cfstore "github.com/micro/micro/v2/internal/plugins/store/cloudflare"
	rrmicro "github.com/micro/micro/v2/internal/resolver/api"
	"github.com/micro/micro/v2/internal/stats"
	"github.com/micro/micro/v2/plugin"
)

// 如果执行micro api 命令是，没有通过命令行参数指定这些参数值，则使用下列的默认值
var (
	Name                  = "go.micro.api"                    // 用于设置API网关服务器的名称
	Address               = ":8080"                           // API网关监听的端口号，客户端根据 API 网关的公网 IP 和这个端口号即可与这个 API 网关进行通信
	Handler               = "meta"                            // 用于设置 API 网关的请求处理器，默认是 meta，这些处理器可用于决定请求路由如何处理，Go Micro 支持的所有处理器可以查看这里：micro/go-micro/api/handler
	Resolver              = "micro"                           // 用于将 HTTP 请求路由映射到对应的后端 API 服务接口，默认是 micro，和 handler 类似，你可以到 micro/go-micro/api/resolver 路径下查看 Go Micro 支持的所有解析器
	RPCPath               = "/rpc"
	APIPath               = "/"
	ProxyPath             = "/{service:[a-zA-Z0-9]+}"
	Namespace             = "go.micro"                        // 用于设置 API 服务的命名空间
	Type                  = "api"
	HeaderPrefix          = "X-Micro-"
	EnableRPC             = false
	ACMEProvider          = "autocert"
	ACMEChallengeProvider = "cloudflare"
	ACMECA                = acme.LetsEncryptProductionCA
)

// 在该函数中，首先读取命令参数并将其赋值给全局变量，比如 address、handler、name（server_name）、resolver、namespace 等
func run(ctx *cli.Context, srvOpts ...micro.Option) {
	log.Init(log.WithFields(map[string]interface{}{"service": "api"}))

	if len(ctx.String("server_name")) > 0 {
		Name = ctx.String("server_name")
	}
	if len(ctx.String("address")) > 0 {
		Address = ctx.String("address")
	}
	if len(ctx.String("handler")) > 0 {
		Handler = ctx.String("handler")
	}
	if len(ctx.String("resolver")) > 0 {
		Resolver = ctx.String("resolver")
	}
	if len(ctx.String("enable_rpc")) > 0 {
		EnableRPC = ctx.Bool("enable_rpc")
	}
	if len(ctx.String("acme_provider")) > 0 {
		ACMEProvider = ctx.String("acme_provider")
	}
	if len(ctx.String("type")) > 0 {
		Type = ctx.String("type")
	}
	if len(ctx.String("namespace")) > 0 {
		// remove the service type from the namespace to allow for
		// backwards compatability
		Namespace = strings.TrimSuffix(ctx.String("namespace"), "."+Type)
	}

	// apiNamespace has the format: "go.micro.api"
	apiNamespace := Namespace + "." + Type

	// Init plugins
	for _, p := range Plugins() {
		p.Init(ctx)
	}

	// Init API
	var opts []server.Option

	// 根据是否设置 enable_acme 或 enable_tls 参数对服务器进行初始化设置，决定是否要启用 HTTPS，以及为哪些服务器启用。
	if ctx.Bool("enable_acme") {
		hosts := helper.ACMEHosts(ctx)
		opts = append(opts, server.EnableACME(true))
		opts = append(opts, server.ACMEHosts(hosts...))
		switch ACMEProvider {
		case "autocert":
			opts = append(opts, server.ACMEProvider(autocert.NewProvider()))
		case "certmagic":
			if ACMEChallengeProvider != "cloudflare" {
				log.Fatal("The only implemented DNS challenge provider is cloudflare")
			}
			apiToken, accountID := os.Getenv("CF_API_TOKEN"), os.Getenv("CF_ACCOUNT_ID")
			kvID := os.Getenv("KV_NAMESPACE_ID")
			if len(apiToken) == 0 || len(accountID) == 0 {
				log.Fatal("env variables CF_API_TOKEN and CF_ACCOUNT_ID must be set")
			}
			if len(kvID) == 0 {
				log.Fatal("env var KV_NAMESPACE_ID must be set to your cloudflare workers KV namespace ID")
			}

			cloudflareStore := cfstore.NewStore(
				cfstore.Token(apiToken),
				cfstore.Account(accountID),
				cfstore.Namespace(kvID),
				cfstore.CacheTTL(time.Minute),
			)
			storage := certmagic.NewStorage(
				memory.NewSync(),
				cloudflareStore,
			)
			config := cloudflare.NewDefaultConfig()
			config.AuthToken = apiToken
			config.ZoneToken = apiToken
			challengeProvider, err := cloudflare.NewDNSProviderConfig(config)
			if err != nil {
				log.Fatal(err.Error())
			}

			opts = append(opts,
				server.ACMEProvider(
					certmagic.NewProvider(
						acme.AcceptToS(true),
						acme.CA(ACMECA),
						acme.Cache(storage),
						acme.ChallengeProvider(challengeProvider),
						acme.OnDemand(false),
					),
				),
			)
		default:
			log.Fatalf("%s is not a valid ACME provider\n", ACMEProvider)
		}
	} else if ctx.Bool("enable_tls") {
		config, err := helper.TLSConfig(ctx)
		if err != nil {
			fmt.Println(err.Error())
			return
		}

		opts = append(opts, server.EnableTLS(true))
		opts = append(opts, server.TLSConfig(config))
	}

	if ctx.Bool("enable_cors") {
		opts = append(opts, server.EnableCORS(true))
	}

	// create the router
	// Micro API 底层基于 gorilla/mux 包实现 HTTP 请求路由的分发
	// 1.首先我们基于 mux 的 NewRouter 函数创建一个路由器并将其赋值给 HTTP 处理器 h（r 是一个指针类型，所以 h 指向的是 r 的引用）
	var h http.Handler
	r := mux.NewRouter()
	h = r

	if ctx.Bool("enable_stats") {
		st := stats.New()
		r.HandleFunc("/stats", st.StatsHandler)
		h = st.ServeHTTP(r)
		st.Start()
		defer st.Stop()
	}

	// return version and list of services
	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "OPTIONS" {
			return
		}

		response := fmt.Sprintf(`{"version": "%s"}`, ctx.App.Version)
		w.Write([]byte(response))
	})

	// strip favicon.ico
	r.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {})

	srvOpts = append(srvOpts, micro.Name(Name))
	if i := time.Duration(ctx.Int("register_ttl")); i > 0 {
		srvOpts = append(srvOpts, micro.RegisterTTL(i*time.Second))
	}
	if i := time.Duration(ctx.Int("register_interval")); i > 0 {
		srvOpts = append(srvOpts, micro.RegisterInterval(i*time.Second))
	}

	// initialise service
	// 2.然后经过一些服务器全局参数的设置之后，传入这些全局参数来初始化服务
	service := micro.NewService(srvOpts...)

	// register rpc handler
	// 3.接下来，注册RPC请求处理器
	// 默认 RPC 请求路径是 /rpc
	// 客户端需要通过这个路径发起 POST 请求，请求参数一般是 JSON 格式数据或者编码过的 RPC 表单请求，例如：
	// 详见：https://micro.mu/docs/cn/api.html

	//curl -d 'service=go.micro.srv.greeter' \
	//-d 'method=Say.Hello' \
	//-d 'request={"name": "John"}' \
    //http://localhost:8080/rpc
    // 或
	//curl -H 'Content-Type: application/json' \
	//-d '{"service": "go.micro.srv.greeter", "method": "Say.Hello", "request": {"name": "John"}}' \
    //http://localhost:8080/rpc

	// 处理器底层会将 RPC 请求转化为对 Go Micro 底层服务的请求，对应的处理器源码位于 micro/micro/internal/handler/rpc.go 中。
	// 这里应该是会绕过 micor-api 的 handler，直接由Handler.RPC进行处理
	if EnableRPC {
		log.Infof("Registering RPC Handler at %s", RPCPath)
		r.HandleFunc(RPCPath, handler.RPC)
	}

	// create the namespace resolver
	nsResolver := namespace.NewResolver(Type, Namespace)

	// resolver options
	// 解析器参数
	ropts := []resolver.Option{
		resolver.WithNamespace(nsResolver.Resolve),
		resolver.WithHandler(Handler),
	}

	// default resolver
	// 4.初始化默认路由解析器，默认解析路由解析器是“micro” 对应源码位于 micro/go-micro/api/resolver/micro/micro.go
	rr := rrmicro.NewResolver(ropts...)

	// Resolver是解析器名称，默认是micro，也可以通过命令行参数指定
	switch Resolver {
	case "host":
		rr = host.NewResolver(ropts...)
	case "path":
		rr = path.NewResolver(ropts...)
	case "grpc":
		rr = grpc.NewResolver(ropts...)
	}

	// Handler是 API 请求处理器，默认是meta
	// 5.注册API请求处理器
	// 默认的命名空间是 go.micro.api，默认的解析器是 micro（对应源码位于 micro/go-micro/api/resolver/micro/micro.go）
	// 然后会传入上述初始化的参数到 regRouter.NewRouter 函数来创建新的 API 路由器（对应源码位于 micro/go-micro/api/router/registry/registry.go）
	// 再通过这个路由器实例和之前初始化的服务实例（包含默认 Registry、Transport、Broker、Client、Server 配置， 以便后续通过这些配置根据服务名和请求参数对底层服务发起请求）来创建 API 处理器
	// （对应源码位于 micro/go-micro/api/handler/api/api.go）
	// 最后，把 API 请求路径前缀和 API 处理器设置到之前创建的路由器 r 上。
	switch Handler {
	case "rpc":
		log.Infof("Registering API RPC Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithHandler(arpc.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		rp := arpc.NewHandler(
			ahandler.WithNamespace(apiNamespace),
			ahandler.WithRouter(rt),
			ahandler.WithClient(service.Client()),
		)
		r.PathPrefix(APIPath).Handler(rp)
	case "api":
		log.Infof("Registering API Request Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithHandler(aapi.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		ap := aapi.NewHandler(
			ahandler.WithNamespace(apiNamespace),
			ahandler.WithRouter(rt),
			ahandler.WithClient(service.Client()),
		)
		r.PathPrefix(APIPath).Handler(ap)
	case "event":
		log.Infof("Registering API Event Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithHandler(event.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		ev := event.NewHandler(
			ahandler.WithNamespace(apiNamespace),
			ahandler.WithRouter(rt),
			ahandler.WithClient(service.Client()),
		)
		r.PathPrefix(APIPath).Handler(ev)
	case "http", "proxy":
		log.Infof("Registering API HTTP Handler at %s", ProxyPath)
		rt := regRouter.NewRouter(
			router.WithHandler(ahttp.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		ht := ahttp.NewHandler(
			ahandler.WithNamespace(apiNamespace),
			ahandler.WithRouter(rt),
			ahandler.WithClient(service.Client()),
		)
		r.PathPrefix(ProxyPath).Handler(ht)
	case "web":
		log.Infof("Registering API Web Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithHandler(web.Handler),
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		w := web.NewHandler(
			ahandler.WithNamespace(apiNamespace),
			ahandler.WithRouter(rt),
			ahandler.WithClient(service.Client()),
		)
		r.PathPrefix(APIPath).Handler(w)
	default:
		log.Infof("Registering API Default Handler at %s", APIPath)
		rt := regRouter.NewRouter(
			router.WithResolver(rr),
			router.WithRegistry(service.Options().Registry),
		)
		r.PathPrefix(APIPath).Handler(handler.Meta(service, rt, nsResolver.Resolve))
	}

	// reverse wrap handler
	plugins := append(Plugins(), plugin.Plugins()...)
	for i := len(plugins); i > 0; i-- {
		h = plugins[i-1].Handler()(h)
	}

	// create the auth wrapper and the server
	// 6.最后，我们初始化用作 API 网关的 HTTP 服务器并启动它（对应源码位于 micro/go-micro/api/server/http/http.go）

	// 当有 HTTP 请求过来时，该网关服务器就可以对其进行解析（通过上述初始化的 Resolver）和处理（通过 API 请求处理器处理）并将结果返回给客户端
	// （相应源码位于 micro/go-micro/api/handler/api/api.go 的 ServeHTTP 方法，以协程方式启动服务器对客户端请求进行处理，底层服务调用逻辑和我们前面介绍的客户端服务发现原理一致）
	// 以上就是 Micro API 网关的底层实现源码，我们可以看到这个默认的 API 网关采用的是 API 网关架构模式的第一种模式：单节点网关模式，所有的 API 请求都会经过这个单一入口对底层服务进行请求。
	authWrapper := auth.Wrapper(rr, nsResolver)
	api := httpapi.NewServer(Address, server.WrapHandler(authWrapper))

	api.Init(opts...)
	api.Handle("/", h)

	// Start API
	// 这个进程是用户接受客户端发来的HTTP请求的
	// 这个进程是开启一个新的协程去启动HTTP服务器
	if err := api.Start(); err != nil {
		log.Fatal(err)
	}

	// Run server
	// 这个进程是用于后续通过 api进程 解析出的配置(服务名和请求参数)对底层服务发起请求
	// 这个进程是在主协程启动服务器，启动后，逻辑会阻塞在这里
	if err := service.Run(); err != nil {
		log.Fatal(err)
	}

	// Stop API
	// 只有service停止之后，这里才会执行
	if err := api.Stop(); err != nil {
		log.Fatal(err)
	}

	// 当有 HTTP 请求过来时，该网关服务器就可以对其进行解析（通过上述初始化的 Resolver）和处理（通过 API 请求处理器处理）
	// 并将结果返回给客户端（相应源码位于 micro/go-micro/api/handler/api/api.go 的 ServeHTTP 方法，以协程方式启动服务器对客户端请求进行处理，底层服务调用逻辑和我们前面介绍的客户端服务发现原理一致）
	// 以上就是 Micro API 网关的底层实现源码，我们可以看到这个默认的 API 网关采用的是 API 网关架构模式的第一种模式：单节点网关模式，所有的 API 请求都会经过这个单一入口对底层服务进行请求。
}

func Commands(options ...micro.Option) []*cli.Command {
	command := &cli.Command{
		Name:  "api",
		Usage: "Run the api gateway",
		Action: func(ctx *cli.Context) error {
			run(ctx, options...)
			return nil
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "address",
				Usage:   "Set the api address e.g 0.0.0.0:8080",
				EnvVars: []string{"MICRO_API_ADDRESS"},
			},
			&cli.StringFlag{
				Name:    "handler",
				Usage:   "Specify the request handler to be used for mapping HTTP requests to services; {api, event, http, rpc}",
				EnvVars: []string{"MICRO_API_HANDLER"},
			},
			&cli.StringFlag{
				Name:    "namespace",
				Usage:   "Set the namespace used by the API e.g. com.example",
				EnvVars: []string{"MICRO_API_NAMESPACE"},
			},
			&cli.StringFlag{
				Name:    "type",
				Usage:   "Set the service type used by the API e.g. api",
				EnvVars: []string{"MICRO_API_TYPE"},
			},
			&cli.StringFlag{
				Name:    "resolver",
				Usage:   "Set the hostname resolver used by the API {host, path, grpc}",
				EnvVars: []string{"MICRO_API_RESOLVER"},
			},
			&cli.BoolFlag{
				Name:    "enable_rpc",
				Usage:   "Enable call the backend directly via /rpc",
				EnvVars: []string{"MICRO_API_ENABLE_RPC"},
			},
			&cli.BoolFlag{
				Name:    "enable_cors",
				Usage:   "Enable CORS, allowing the API to be called by frontend applications",
				EnvVars: []string{"MICRO_API_ENABLE_CORS"},
				Value:   true,
			},
		},
	}

	for _, p := range Plugins() {
		if cmds := p.Commands(); len(cmds) > 0 {
			command.Subcommands = append(command.Subcommands, cmds...)
		}

		if flags := p.Flags(); len(flags) > 0 {
			command.Flags = append(command.Flags, flags...)
		}
	}

	return []*cli.Command{command}
}
