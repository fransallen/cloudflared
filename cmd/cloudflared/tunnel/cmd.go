package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"runtime/trace"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflared/awsuploader"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/buildinfo"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/config"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/ui"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/updater"
	"github.com/cloudflare/cloudflared/dbconnect"
	"github.com/cloudflare/cloudflared/h2mux"
	"github.com/cloudflare/cloudflared/hello"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/logger"
	"github.com/cloudflare/cloudflared/metrics"
	"github.com/cloudflare/cloudflared/origin"
	"github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/socks"
	"github.com/cloudflare/cloudflared/sshlog"
	"github.com/cloudflare/cloudflared/sshserver"
	"github.com/cloudflare/cloudflared/tlsconfig"
	"github.com/cloudflare/cloudflared/tunneldns"
	"github.com/cloudflare/cloudflared/tunnelstore"
	"github.com/cloudflare/cloudflared/websocket"

	"github.com/coreos/go-systemd/daemon"
	"github.com/facebookgo/grace/gracenet"
	"github.com/getsentry/raven-go"
	"github.com/gliderlabs/ssh"
	"github.com/google/uuid"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"
)

const (
	sentryDSN = "https://56a9c9fa5c364ab28f34b14f35ea0f1b:3e8827f6f9f740738eb11138f7bebb68@sentry.io/189878"

	sshLogFileDirectory = "/usr/local/var/log/cloudflared/"

	// sshPortFlag is the port on localhost the cloudflared ssh server will run on
	sshPortFlag = "local-ssh-port"

	// sshIdleTimeoutFlag defines the duration a SSH session can remain idle before being closed
	sshIdleTimeoutFlag = "ssh-idle-timeout"

	// sshMaxTimeoutFlag defines the max duration a SSH session can remain open for
	sshMaxTimeoutFlag = "ssh-max-timeout"

	// bucketNameFlag is the bucket name to use for the SSH log uploader
	bucketNameFlag = "bucket-name"

	// regionNameFlag is the AWS region name to use for the SSH log uploader
	regionNameFlag = "region-name"

	// secretIDFlag is the Secret id of SSH log uploader
	secretIDFlag = "secret-id"

	// accessKeyIDFlag is the Access key id of SSH log uploader
	accessKeyIDFlag = "access-key-id"

	// sessionTokenIDFlag is the Session token of SSH log uploader
	sessionTokenIDFlag = "session-token"

	// s3URLFlag is the S3 URL of SSH log uploader (e.g. don't use AWS s3 and use google storage bucket instead)
	s3URLFlag = "s3-url-host"

	// hostKeyPath is the path of the dir to save SSH host keys too
	hostKeyPath = "host-key-path"

	//sshServerFlag enables cloudflared ssh proxy server
	sshServerFlag = "ssh-server"

	// socks5Flag is to enable the socks server to deframe
	socks5Flag = "socks5"

	// bastionFlag is to enable bastion, or jump host, operation
	bastionFlag = "bastion"

	// uiFlag is to enable launching cloudflared in interactive UI mode
	uiFlag = "ui"

	logDirectoryFlag = "log-directory"

	debugLevelWarning = "At debug level, request URL, method, protocol, content legnth and header will be logged. " +
		"Response status, content length and header will also be logged in debug level."
)

var (
	shutdownC      chan struct{}
	graceShutdownC chan struct{}
	version        string
)

func Flags() []cli.Flag {
	return tunnelFlags(true)
}

func Commands() []*cli.Command {
	subcommands := []*cli.Command{
		buildLoginSubcommand(false),
		buildCreateCommand(),
		buildRouteCommand(),
		buildRunCommand(),
		buildListCommand(),
		buildIngressSubcommand(),
		buildDeleteCommand(),
		buildCleanupCommand(),
		// for compatibility, allow following as tunnel subcommands
		tunneldns.Command(true),
		dbConnectCmd(),
	}

	return []*cli.Command{
		buildTunnelCommand(subcommands),
		// for compatibility, allow following as top-level subcommands
		buildLoginSubcommand(true),
		dbConnectCmd(),
	}
}

func buildTunnelCommand(subcommands []*cli.Command) *cli.Command {
	return &cli.Command{
		Name:      "tunnel",
		Action:    cliutil.ErrorHandler(TunnelCommand),
		Before:    SetFlagsFromConfigFile,
		Category:  "Tunnel",
		Usage:     "Make a locally-running web service accessible over the internet using Argo Tunnel.",
		ArgsUsage: " ",
		Description: `Argo Tunnel asks you to specify a hostname on a Cloudflare-powered
		domain you control and a local address. Traffic from that hostname is routed
		(optionally via a Cloudflare Load Balancer) to this machine and appears on the
		specified port where it can be served.

		This feature requires your Cloudflare account be subscribed to the Argo Smart Routing feature.

		To use, begin by calling login to download a certificate:

			$ cloudflared tunnel login

		With your certificate installed you can then launch your first tunnel,
		replacing my.site.com with a subdomain of your site:

			$ cloudflared tunnel --hostname my.site.com --url http://localhost:8080

		If you have a web server running on port 8080 (in this example), it will be available on
		the internet!`,
		Subcommands: subcommands,
		Flags:       tunnelFlags(false),
	}
}

func buildIngressSubcommand() *cli.Command {
	return &cli.Command{
		Name:     "ingress",
		Category: "Tunnel",
		Usage:    "Validate and test cloudflared tunnel's ingress configuration",
		Hidden:   true,
		Description: `
		Cloudflared lets you route traffic from the internet to multiple different addresses on your
		origin. Multiple-origin routing is configured by a set of rules. Each rule matches traffic
		by its hostname or path, and routes it to an address. These rules are configured under the
		'ingress' key of your config.yaml, for example:

		ingress:
		  - hostname: www.example.com
		    service: https://localhost:8000
		  - hostname: *.example.xyz
		    path: /[a-zA-Z]+.html
		    service: https://localhost:8001
		  - hostname: *
		    service: https://localhost:8002

		To ensure cloudflared can route all incoming requests, the last rule must be a catch-all
		rule that matches all traffic. You can validate these rules with the 'ingress validate'
		command, and test which rule matches a particular URL with 'ingress rule <URL>'.

		Multiple-origin routing is incompatible with the --url flag.`,
		Subcommands: []*cli.Command{buildValidateCommand(), buildRuleCommand()},
		Flags:       tunnelFlags(false),
	}
}

func TunnelCommand(c *cli.Context) error {
	if name := c.String("name"); name != "" { // Start a named tunnel
		return adhocNamedTunnel(c, name)
	} else { // Start a classic tunnel
		return classicTunnel(c)
	}
}

func Init(v string, s, g chan struct{}) {
	version, shutdownC, graceShutdownC = v, s, g
}

// adhocNamedTunnel create, route and run a named tunnel in one command
func adhocNamedTunnel(c *cli.Context, name string) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	tunnel, ok, err := sc.tunnelActive(name)
	if err != nil || !ok {
		tunnel, err = sc.create(name)
		if err != nil {
			return errors.Wrap(err, "failed to create tunnel")
		}
	} else {
		sc.logger.Infof("Tunnel already created with ID %s", tunnel.ID)
	}

	if r, ok := routeFromFlag(c); ok {
		if res, err := sc.route(tunnel.ID, r); err != nil {
			sc.logger.Errorf("failed to create route, please create it manually. err: %v.", err)
		} else {
			sc.logger.Infof(res.SuccessSummary())
		}
	}

	if err := sc.run(tunnel.ID); err != nil {
		return errors.Wrap(err, "error running tunnel")
	}

	return nil
}

// classicTunnel creates a "classic" non-named tunnel
func classicTunnel(c *cli.Context) error {
	sc, err := newSubcommandContext(c)
	if err != nil {
		return err
	}

	return StartServer(c, version, shutdownC, graceShutdownC, nil, sc.logger, sc.isUIEnabled)
}

func routeFromFlag(c *cli.Context) (tunnelstore.Route, bool) {
	if hostname := c.String("hostname"); hostname != "" {
		if lbPool := c.String("lb-pool"); lbPool != "" {
			return tunnelstore.NewLBRoute(hostname, lbPool), true
		}
		return tunnelstore.NewDNSRoute(hostname), true
	}
	return nil, false
}

func createLogger(c *cli.Context, isTransport bool, disableTerminal bool) (*logger.OutputWriter, error) {
	var loggerOpts []logger.Option

	logPath := c.String("logfile")
	if logPath == "" {
		logPath = c.String(logDirectoryFlag)
	}

	if logPath != "" {
		loggerOpts = append(loggerOpts, logger.DefaultFile(logPath))
	}

	logLevel := c.String("loglevel")
	if isTransport {
		logLevel = c.String("transport-loglevel")
		if logLevel == "" {
			logLevel = "fatal"
		}
	}
	loggerOpts = append(loggerOpts, logger.LogLevelString(logLevel))

	if disableTerminal {
		disableOption := logger.DisableTerminal(true)
		loggerOpts = append(loggerOpts, disableOption)
	}

	l, err := logger.New(loggerOpts...)
	if err != nil {
		return nil, err
	}

	return l, nil
}

func StartServer(
	c *cli.Context,
	version string,
	shutdownC,
	graceShutdownC chan struct{},
	namedTunnel *origin.NamedTunnelConfig,
	log logger.Service,
	isUIEnabled bool,
) error {
	_ = raven.SetDSN(sentryDSN)
	var wg sync.WaitGroup
	listeners := gracenet.Net{}
	errC := make(chan error)
	connectedSignal := signal.New(make(chan struct{}))
	dnsReadySignal := make(chan struct{})

	if c.String("config") == "" {
		log.Infof(config.ErrNoConfigFile.Error())
	}

	if c.IsSet("trace-output") {
		tmpTraceFile, err := ioutil.TempFile("", "trace")
		if err != nil {
			log.Errorf("Failed to create new temporary file to save trace output: %s", err)
		}

		defer func() {
			if err := tmpTraceFile.Close(); err != nil {
				log.Errorf("Failed to close trace output file %s with error: %s", tmpTraceFile.Name(), err)
			}
			if err := os.Rename(tmpTraceFile.Name(), c.String("trace-output")); err != nil {
				log.Errorf("Failed to rename temporary trace output file %s to %s with error: %s", tmpTraceFile.Name(), c.String("trace-output"), err)
			} else {
				err := os.Remove(tmpTraceFile.Name())
				if err != nil {
					log.Errorf("Failed to remove the temporary trace file %s with error: %s", tmpTraceFile.Name(), err)
				}
			}
		}()

		if err := trace.Start(tmpTraceFile); err != nil {
			log.Errorf("Failed to start trace: %s", err)
			return errors.Wrap(err, "Error starting tracing")
		}
		defer trace.Stop()
	}

	buildInfo := buildinfo.GetBuildInfo(version)
	buildInfo.Log(log)
	logClientOptions(c, log)

	if c.IsSet("proxy-dns") {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errC <- runDNSProxyServer(c, dnsReadySignal, shutdownC, log)
		}()
	} else {
		close(dnsReadySignal)
	}

	// Wait for proxy-dns to come up (if used)
	<-dnsReadySignal

	metricsListener, err := listeners.Listen("tcp", c.String("metrics"))
	if err != nil {
		log.Errorf("Error opening metrics server listener: %s", err)
		return errors.Wrap(err, "Error opening metrics server listener")
	}
	defer metricsListener.Close()
	wg.Add(1)
	go func() {
		defer wg.Done()
		errC <- metrics.ServeMetrics(metricsListener, shutdownC, log)
	}()

	go notifySystemd(connectedSignal)
	if c.IsSet("pidfile") {
		go writePidFile(connectedSignal, c.String("pidfile"), log)
	}

	cloudflaredID, err := uuid.NewRandom()
	if err != nil {
		log.Errorf("Cannot generate cloudflared ID: %s", err)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-shutdownC
		cancel()
	}()

	// update needs to be after DNS proxy is up to resolve equinox server address
	if updater.IsAutoupdateEnabled(c, log) {
		log.Infof("Autoupdate frequency is set to %v", c.Duration("autoupdate-freq"))
		wg.Add(1)
		go func() {
			defer wg.Done()
			autoupdater := updater.NewAutoUpdater(c.Duration("autoupdate-freq"), &listeners, log)
			errC <- autoupdater.Run(ctx)
		}()
	}

	// Serve DNS proxy stand-alone if no hostname or tag or app is going to run
	if dnsProxyStandAlone(c) {
		connectedSignal.Notify()
		// no grace period, handle SIGINT/SIGTERM immediately
		return waitToShutdown(&wg, errC, shutdownC, graceShutdownC, 0, log)
	}

	if c.IsSet("hello-world") {
		log.Infof("hello-world set")
		helloListener, err := hello.CreateTLSListener("127.0.0.1:")
		if err != nil {
			log.Errorf("Cannot start Hello World Server: %s", err)
			return errors.Wrap(err, "Cannot start Hello World Server")
		}
		defer helloListener.Close()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = hello.StartHelloWorldServer(log, helloListener, shutdownC)
		}()
		forceSetFlag(c, "url", "https://"+helloListener.Addr().String())
	}

	if c.IsSet(sshServerFlag) {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			msg := fmt.Sprintf("--ssh-server is not supported on %s", runtime.GOOS)
			log.Error(msg)
			return errors.New(msg)
		}

		log.Infof("ssh-server set")

		logManager := sshlog.NewEmptyManager()
		if c.IsSet(bucketNameFlag) && c.IsSet(regionNameFlag) && c.IsSet(accessKeyIDFlag) && c.IsSet(secretIDFlag) {
			uploader, err := awsuploader.NewFileUploader(c.String(bucketNameFlag), c.String(regionNameFlag),
				c.String(accessKeyIDFlag), c.String(secretIDFlag), c.String(sessionTokenIDFlag), c.String(s3URLFlag))
			if err != nil {
				msg := "Cannot create uploader for SSH Server"
				log.Errorf("%s: %s", msg, err)
				return errors.Wrap(err, msg)
			}

			if err := os.MkdirAll(sshLogFileDirectory, 0700); err != nil {
				msg := fmt.Sprintf("Cannot create SSH log file directory %s", sshLogFileDirectory)
				log.Errorf("%s: %s", msg, err)
				return errors.Wrap(err, msg)
			}

			logManager = sshlog.New(sshLogFileDirectory)

			uploadManager := awsuploader.NewDirectoryUploadManager(log, uploader, sshLogFileDirectory, 30*time.Minute, shutdownC)
			uploadManager.Start()
		}

		localServerAddress := "127.0.0.1:" + c.String(sshPortFlag)
		server, err := sshserver.New(logManager, log, version, localServerAddress, c.String("hostname"), c.Path(hostKeyPath), shutdownC, c.Duration(sshIdleTimeoutFlag), c.Duration(sshMaxTimeoutFlag))
		if err != nil {
			msg := "Cannot create new SSH Server"
			log.Errorf("%s: %s", msg, err)
			return errors.Wrap(err, msg)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err = server.Start(); err != nil && err != ssh.ErrServerClosed {
				log.Errorf("SSH server error: %s", err)
				// TODO: remove when declarative tunnels are implemented.
				close(shutdownC)
			}
		}()
		forceSetFlag(c, "url", "ssh://"+localServerAddress)
	}

	url := c.String("url")
	hostname := c.String("hostname")
	if url == hostname && url != "" && hostname != "" {
		errText := "hostname and url shouldn't match. See --help for more information"
		log.Error(errText)
		return fmt.Errorf(errText)
	}

	if staticHost := hostnameFromURI(c.String("url")); isProxyDestinationConfigured(staticHost, c) {
		listener, err := net.Listen("tcp", net.JoinHostPort(c.String("proxy-address"), strconv.Itoa(c.Int("proxy-port"))))
		if err != nil {
			log.Errorf("Cannot start Websocket Proxy Server: %s", err)
			return errors.Wrap(err, "Cannot start Websocket Proxy Server")
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			streamHandler := websocket.DefaultStreamHandler
			if c.IsSet(socks5Flag) {
				log.Info("SOCKS5 server started")
				streamHandler = func(wsConn *websocket.Conn, remoteConn net.Conn, _ http.Header) {
					dialer := socks.NewConnDialer(remoteConn)
					requestHandler := socks.NewRequestHandler(dialer)
					socksServer := socks.NewConnectionHandler(requestHandler)

					socksServer.Serve(wsConn)
				}
			} else if c.IsSet(sshServerFlag) {
				streamHandler = func(wsConn *websocket.Conn, remoteConn net.Conn, requestHeaders http.Header) {
					if finalDestination := requestHeaders.Get(h2mux.CFJumpDestinationHeader); finalDestination != "" {
						token := requestHeaders.Get(h2mux.CFAccessTokenHeader)
						if err := websocket.SendSSHPreamble(remoteConn, finalDestination, token); err != nil {
							log.Errorf("Failed to send SSH preamble: %s", err)
							return
						}
					}
					websocket.DefaultStreamHandler(wsConn, remoteConn, requestHeaders)
				}
			}
			errC <- websocket.StartProxyServer(log, listener, staticHost, shutdownC, streamHandler)
		}()
		forceSetFlag(c, "url", "http://"+listener.Addr().String())
	}

	transportLogger, err := createLogger(c, true, false)
	if err != nil {
		return errors.Wrap(err, "error setting up transport logger")
	}

	tunnelConfig, err := prepareTunnelConfig(c, buildInfo, version, log, transportLogger, namedTunnel)
	if err != nil {
		return err
	}

	reconnectCh := make(chan origin.ReconnectSignal, 1)
	if c.IsSet("stdin-control") {
		log.Info("Enabling control through stdin")
		go stdinControl(reconnectCh, log)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		errC <- origin.StartTunnelDaemon(ctx, tunnelConfig, connectedSignal, cloudflaredID, reconnectCh)
	}()

	if isUIEnabled {
		const tunnelEventChanBufferSize = 16
		tunnelEventChan := make(chan ui.TunnelEvent, tunnelEventChanBufferSize)
		tunnelConfig.TunnelEventChan = tunnelEventChan

		tunnelInfo := ui.NewUIModel(
			version,
			hostname,
			metricsListener.Addr().String(),
			tunnelConfig.OriginUrl,
			tunnelConfig.HAConnections,
		)
		logLevels, err := logger.ParseLevelString(c.String("loglevel"))
		if err != nil {
			return err
		}
		tunnelInfo.LaunchUI(ctx, log, logLevels, tunnelEventChan)
	}

	return waitToShutdown(&wg, errC, shutdownC, graceShutdownC, c.Duration("grace-period"), log)
}

// forceSetFlag attempts to set the given flag value in the closest context that has it defined
func forceSetFlag(c *cli.Context, name, value string) {
	for _, ctx := range c.Lineage() {
		if err := ctx.Set(name, value); err == nil {
			break
		}
	}
}

func SetFlagsFromConfigFile(c *cli.Context) error {
	logger, err := createLogger(c, false, false)
	if err != nil {
		return cliutil.PrintLoggerSetupError("error setting up logger", err)
	}

	configFile := c.String("config")
	if configFile == "" {
		logger.Debugf(config.ErrNoConfigFile.Error())
		return nil
	}

	inputSource, err := altsrc.NewYamlSourceFromFile(configFile)
	if err != nil {
		logger.Errorf("Cannot load configuration from %s: %s", configFile, err)
		return err
	}
	if inputSource != nil {
		targetFlags := c.Command.Flags
		if c.Command.Name == "" {
			targetFlags = c.App.Flags
		}
		err := altsrc.ApplyInputSourceValues(c, inputSource, targetFlags)
		if err != nil {
			logger.Errorf("Cannot apply configuration from %s: %s", configFile, err)
			return err
		}
		logger.Debugf("Applied configuration from %s", configFile)
	}
	return nil
}

// isProxyDestinationConfigured returns true if there is a static host set or if bastion mode is set.
func isProxyDestinationConfigured(staticHost string, c *cli.Context) bool {
	return staticHost != "" || c.IsSet(bastionFlag)
}

func waitToShutdown(wg *sync.WaitGroup,
	errC chan error,
	shutdownC, graceShutdownC chan struct{},
	gracePeriod time.Duration,
	logger logger.Service,
) error {
	var err error
	if gracePeriod > 0 {
		err = waitForSignalWithGraceShutdown(errC, shutdownC, graceShutdownC, gracePeriod, logger)
	} else {
		err = waitForSignal(errC, shutdownC, logger)
		close(graceShutdownC)
	}

	if err != nil {
		logger.Errorf("Quitting due to error: %s", err)
	} else {
		logger.Info("Quitting...")
	}
	// Wait for clean exit, discarding all errors
	go func() {
		for range errC {
		}
	}()
	wg.Wait()
	return err
}

func notifySystemd(waitForSignal *signal.Signal) {
	<-waitForSignal.Wait()
	daemon.SdNotify(false, "READY=1")
}

func writePidFile(waitForSignal *signal.Signal, pidFile string, logger logger.Service) {
	<-waitForSignal.Wait()
	expandedPath, err := homedir.Expand(pidFile)
	if err != nil {
		logger.Errorf("Unable to expand %s, try to use absolute path in --pidfile: %s", pidFile, err)
		return
	}
	file, err := os.Create(expandedPath)
	if err != nil {
		logger.Errorf("Unable to write pid to %s: %s", expandedPath, err)
		return
	}
	defer file.Close()
	fmt.Fprintf(file, "%d", os.Getpid())
}

func hostnameFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "ssh":
		return addPortIfMissing(u, 22)
	case "rdp":
		return addPortIfMissing(u, 3389)
	case "smb":
		return addPortIfMissing(u, 445)
	case "tcp":
		return addPortIfMissing(u, 7864) // just a random port since there isn't a default in this case
	}
	return ""
}

func addPortIfMissing(uri *url.URL, port int) string {
	if uri.Port() != "" {
		return uri.Host
	}
	return fmt.Sprintf("%s:%d", uri.Hostname(), port)
}

func dbConnectCmd() *cli.Command {
	cmd := dbconnect.Cmd()

	// Append the tunnel commands so users can customize the daemon settings.
	cmd.Flags = appendFlags(Flags(), cmd.Flags...)

	// Override before to run tunnel validation before dbconnect validation.
	cmd.Before = func(c *cli.Context) error {
		err := SetFlagsFromConfigFile(c)
		if err == nil {
			err = dbconnect.CmdBefore(c)
		}
		return err
	}

	// Override action to setup the Proxy, then if successful, start the tunnel daemon.
	cmd.Action = cliutil.ErrorHandler(func(c *cli.Context) error {
		err := dbconnect.CmdAction(c)
		if err == nil {
			err = TunnelCommand(c)
		}
		return err
	})

	return cmd
}

// appendFlags will append extra flags to a slice of flags.
//
// The cli package will panic if two flags exist with the same name,
// so if extraFlags contains a flag that was already defined, modify the
// original flags to use the extra version.
func appendFlags(flags []cli.Flag, extraFlags ...cli.Flag) []cli.Flag {
	for _, extra := range extraFlags {
		var found bool

		// Check if an extra flag overrides an existing flag.
		for i, flag := range flags {
			if reflect.DeepEqual(extra.Names(), flag.Names()) {
				flags[i] = extra
				found = true
				break
			}
		}

		// Append the extra flag if it has nothing to override.
		if !found {
			flags = append(flags, extra)
		}
	}

	return flags
}

func tunnelFlags(shouldHide bool) []cli.Flag {
	flags := configureCloudflaredFlags(shouldHide)
	flags = append(flags, configureProxyFlags(shouldHide)...)
	flags = append(flags, configureLoggingFlags(shouldHide)...)
	flags = append(flags, configureProxyDNSFlags(shouldHide)...)
	flags = append(flags, []cli.Flag{
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:   "is-autoupdated",
			Usage:  "Signal the new process that Argo Tunnel client has been autoupdated",
			Value:  false,
			Hidden: true,
		}),
		altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
			Name:    "edge",
			Usage:   "Address of the Cloudflare tunnel server. Only works in Cloudflare's internal testing environment.",
			EnvVars: []string{"TUNNEL_EDGE"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    tlsconfig.CaCertFlag,
			Usage:   "Certificate Authority authenticating connections with Cloudflare's edge network.",
			EnvVars: []string{"TUNNEL_CACERT"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "hostname",
			Usage:   "Set a hostname on a Cloudflare zone to route traffic through this tunnel.",
			EnvVars: []string{"TUNNEL_HOSTNAME"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "id",
			Usage:   "A unique identifier used to tie connections to this tunnel instance.",
			EnvVars: []string{"TUNNEL_ID"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "lb-pool",
			Usage:   "The name of a (new/existing) load balancing pool to add this origin to.",
			EnvVars: []string{"TUNNEL_LB_POOL"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "api-key",
			Usage:   "This parameter has been deprecated since version 2017.10.1.",
			EnvVars: []string{"TUNNEL_API_KEY"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "api-email",
			Usage:   "This parameter has been deprecated since version 2017.10.1.",
			EnvVars: []string{"TUNNEL_API_EMAIL"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "api-ca-key",
			Usage:   "This parameter has been deprecated since version 2017.10.1.",
			EnvVars: []string{"TUNNEL_API_CA_KEY"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "api-url",
			Usage:   "Base URL for Cloudflare API v4",
			EnvVars: []string{"TUNNEL_API_URL"},
			Value:   "https://api.cloudflare.com/client/v4",
			Hidden:  true,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:    "metrics-update-freq",
			Usage:   "Frequency to update tunnel metrics",
			Value:   time.Second * 5,
			EnvVars: []string{"TUNNEL_METRICS_UPDATE_FREQ"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
			Name:    "tag",
			Usage:   "Custom tags used to identify this tunnel, in format `KEY=VALUE`. Multiple tags may be specified",
			EnvVars: []string{"TUNNEL_TAG"},
			Hidden:  shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "heartbeat-interval",
			Usage:  "Minimum idle time before sending a heartbeat.",
			Value:  time.Second * 5,
			Hidden: true,
		}),
		altsrc.NewUint64Flag(&cli.Uint64Flag{
			Name:   "heartbeat-count",
			Usage:  "Minimum number of unacked heartbeats to send before closing the connection.",
			Value:  5,
			Hidden: true,
		}),
		altsrc.NewUintFlag(&cli.UintFlag{
			Name:    "retries",
			Value:   5,
			Usage:   "Maximum number of retries for connection/protocol errors.",
			EnvVars: []string{"TUNNEL_RETRIES"},
			Hidden:  shouldHide,
		}),
		altsrc.NewIntFlag(&cli.IntFlag{
			Name:   "ha-connections",
			Value:  4,
			Hidden: true,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:    "grace-period",
			Usage:   "Duration to accept new requests after cloudflared receives first SIGINT/SIGTERM. A second SIGINT/SIGTERM will force cloudflared to shutdown immediately.",
			Value:   time.Second * 30,
			EnvVars: []string{"TUNNEL_GRACE_PERIOD"},
			Hidden:  true,
		}),
		altsrc.NewUintFlag(&cli.UintFlag{
			Name:    "compression-quality",
			Value:   0,
			Usage:   "(beta) Use cross-stream compression instead HTTP compression. 0-off, 1-low, 2-medium, >=3-high.",
			EnvVars: []string{"TUNNEL_COMPRESSION_LEVEL"},
			Hidden:  shouldHide,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    "use-reconnect-token",
			Usage:   "Test reestablishing connections with the new 'reconnect token' flow.",
			Value:   true,
			EnvVars: []string{"TUNNEL_USE_RECONNECT_TOKEN"},
			Hidden:  true,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:    "dial-edge-timeout",
			Usage:   "Maximum wait time to set up a connection with the edge",
			Value:   time.Second * 15,
			EnvVars: []string{"DIAL_EDGE_TIMEOUT"},
			Hidden:  true,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    "stdin-control",
			Usage:   "Control the process using commands sent through stdin",
			EnvVars: []string{"STDIN-CONTROL"},
			Hidden:  true,
			Value:   false,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "name",
			Aliases: []string{"n"},
			EnvVars: []string{"TUNNEL_NAME"},
			Usage:   "Stable name to identify the tunnel. Using this flag will create, route and run a tunnel. For production usage, execute each command separately",
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:   uiFlag,
			Usage:  "Launch tunnel UI. Tunnel logs are scrollable via 'j', 'k', or arrow keys.",
			Value:  false,
			Hidden: shouldHide,
		}),
	}...)

	return flags
}

// Flags in tunnel command that is relevant to run subcommand
func configureCloudflaredFlags(shouldHide bool) []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:   "config",
			Usage:  "Specifies a config file in YAML format.",
			Value:  config.FindDefaultConfigPath(),
			Hidden: shouldHide,
		},
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "origincert",
			Usage:   "Path to the certificate generated for your origin when you run cloudflared login.",
			EnvVars: []string{"TUNNEL_ORIGIN_CERT"},
			Value:   findDefaultOriginCertPath(),
			Hidden:  shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "autoupdate-freq",
			Usage:  fmt.Sprintf("Autoupdate frequency. Default is %v.", updater.DefaultCheckUpdateFreq),
			Value:  updater.DefaultCheckUpdateFreq,
			Hidden: shouldHide,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    "no-autoupdate",
			Usage:   "Disable periodic check for updates, restarting the server with the new version.",
			EnvVars: []string{"NO_AUTOUPDATE"},
			Value:   false,
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "metrics",
			Value:   "localhost:",
			Usage:   "Listen address for metrics reporting.",
			EnvVars: []string{"TUNNEL_METRICS"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "pidfile",
			Usage:   "Write the application's PID to this file after first successful connection.",
			EnvVars: []string{"TUNNEL_PIDFILE"},
			Hidden:  shouldHide,
		}),
	}
}

func configureProxyFlags(shouldHide bool) []cli.Flag {
	flags := []cli.Flag{
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "url",
			Value:   "http://localhost:8080",
			Usage:   "Connect to the local webserver at `URL`.",
			EnvVars: []string{"TUNNEL_URL"},
			Hidden:  shouldHide,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    "hello-world",
			Value:   false,
			Usage:   "Run Hello World Server",
			EnvVars: []string{"TUNNEL_HELLO_WORLD"},
			Hidden:  shouldHide,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    socks5Flag,
			Usage:   "specify if this tunnel is running as a SOCK5 Server",
			EnvVars: []string{"TUNNEL_SOCKS"},
			Value:   false,
			Hidden:  shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "proxy-connect-timeout",
			Usage:  "HTTP proxy timeout for establishing a new connection",
			Value:  time.Second * 30,
			Hidden: shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "proxy-tls-timeout",
			Usage:  "HTTP proxy timeout for completing a TLS handshake",
			Value:  time.Second * 10,
			Hidden: shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "proxy-tcp-keepalive",
			Usage:  "HTTP proxy TCP keepalive duration",
			Value:  time.Second * 30,
			Hidden: shouldHide,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:   "proxy-no-happy-eyeballs",
			Usage:  "HTTP proxy should disable \"happy eyeballs\" for IPv4/v6 fallback",
			Hidden: shouldHide,
		}),
		altsrc.NewIntFlag(&cli.IntFlag{
			Name:   "proxy-keepalive-connections",
			Usage:  "HTTP proxy maximum keepalive connection pool size",
			Value:  100,
			Hidden: shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "proxy-keepalive-timeout",
			Usage:  "HTTP proxy timeout for closing an idle connection",
			Value:  time.Second * 90,
			Hidden: shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "proxy-connection-timeout",
			Usage:  "HTTP proxy timeout for closing an idle connection",
			Value:  time.Second * 90,
			Hidden: shouldHide,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:   "proxy-expect-continue-timeout",
			Usage:  "HTTP proxy timeout for closing an idle connection",
			Value:  time.Second * 90,
			Hidden: shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "http-host-header",
			Usage:   "Sets the HTTP Host header for the local webserver.",
			EnvVars: []string{"TUNNEL_HTTP_HOST_HEADER"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "origin-server-name",
			Usage:   "Hostname on the origin server certificate.",
			EnvVars: []string{"TUNNEL_ORIGIN_SERVER_NAME"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "unix-socket",
			Usage:   "Path to unix socket to use instead of --url",
			EnvVars: []string{"TUNNEL_UNIX_SOCKET"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    tlsconfig.OriginCAPoolFlag,
			Usage:   "Path to the CA for the certificate of your origin. This option should be used only if your certificate is not signed by Cloudflare.",
			EnvVars: []string{"TUNNEL_ORIGIN_CA_POOL"},
			Hidden:  shouldHide,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    "no-tls-verify",
			Usage:   "Disables TLS verification of the certificate presented by your origin. Will allow any certificate from the origin to be accepted. Note: The connection from your machine to Cloudflare's Edge is still encrypted.",
			EnvVars: []string{"NO_TLS_VERIFY"},
			Hidden:  shouldHide,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    "no-chunked-encoding",
			Usage:   "Disables chunked transfer encoding; useful if you are running a WSGI server.",
			EnvVars: []string{"TUNNEL_NO_CHUNKED_ENCODING"},
			Hidden:  shouldHide,
		}),
	}
	return append(flags, sshFlags(shouldHide)...)
}

func sshFlags(shouldHide bool) []cli.Flag {
	return []cli.Flag{
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    sshPortFlag,
			Usage:   "Localhost port that cloudflared SSH server will run on",
			Value:   "2222",
			EnvVars: []string{"LOCAL_SSH_PORT"},
			Hidden:  true,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:    sshIdleTimeoutFlag,
			Usage:   "Connection timeout after no activity",
			EnvVars: []string{"SSH_IDLE_TIMEOUT"},
			Hidden:  true,
		}),
		altsrc.NewDurationFlag(&cli.DurationFlag{
			Name:    sshMaxTimeoutFlag,
			Usage:   "Absolute connection timeout",
			EnvVars: []string{"SSH_MAX_TIMEOUT"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    bucketNameFlag,
			Usage:   "Bucket name of where to upload SSH logs",
			EnvVars: []string{"BUCKET_ID"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    regionNameFlag,
			Usage:   "Region name of where to upload SSH logs",
			EnvVars: []string{"REGION_ID"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    secretIDFlag,
			Usage:   "Secret ID of where to upload SSH logs",
			EnvVars: []string{"SECRET_ID"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    accessKeyIDFlag,
			Usage:   "Access Key ID of where to upload SSH logs",
			EnvVars: []string{"ACCESS_CLIENT_ID"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    sessionTokenIDFlag,
			Usage:   "Session Token to use in the configuration of SSH logs uploading",
			EnvVars: []string{"SESSION_TOKEN_ID"},
			Hidden:  true,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    s3URLFlag,
			Usage:   "S3 url of where to upload SSH logs",
			EnvVars: []string{"S3_URL"},
			Hidden:  true,
		}),
		altsrc.NewPathFlag(&cli.PathFlag{
			Name:    hostKeyPath,
			Usage:   "Absolute path of directory to save SSH host keys in",
			EnvVars: []string{"HOST_KEY_PATH"},
			Hidden:  true,
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    sshServerFlag,
			Value:   false,
			Usage:   "Run an SSH Server",
			EnvVars: []string{"TUNNEL_SSH_SERVER"},
			Hidden:  true, // TODO: remove when feature is complete
		}),
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    bastionFlag,
			Value:   false,
			Usage:   "Runs as jump host",
			EnvVars: []string{"TUNNEL_BASTION"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "proxy-address",
			Usage:   "Listen address for the proxy.",
			Value:   "127.0.0.1",
			EnvVars: []string{"TUNNEL_PROXY_ADDRESS"},
			Hidden:  shouldHide,
		}),
		altsrc.NewIntFlag(&cli.IntFlag{
			Name:    "proxy-port",
			Usage:   "Listen port for the proxy.",
			Value:   0,
			EnvVars: []string{"TUNNEL_PROXY_PORT"},
			Hidden:  shouldHide,
		}),
	}
}

func configureLoggingFlags(shouldHide bool) []cli.Flag {
	return []cli.Flag{
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "loglevel",
			Value:   "info",
			Usage:   "Application logging level {fatal, error, info, debug}. " + debugLevelWarning,
			EnvVars: []string{"TUNNEL_LOGLEVEL"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "transport-loglevel",
			Aliases: []string{"proto-loglevel"}, // This flag used to be called proto-loglevel
			Value:   "fatal",
			Usage:   "Transport logging level(previously called protocol logging level) {fatal, error, info, debug}",
			EnvVars: []string{"TUNNEL_PROTO_LOGLEVEL", "TUNNEL_TRANSPORT_LOGLEVEL"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "logfile",
			Usage:   "Save application log to this file for reporting issues.",
			EnvVars: []string{"TUNNEL_LOGFILE"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    logDirectoryFlag,
			Usage:   "Save application log to this directory for reporting issues.",
			EnvVars: []string{"TUNNEL_LOGDIRECTORY"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "trace-output",
			Usage:   "Name of trace output file, generated when cloudflared stops.",
			EnvVars: []string{"TUNNEL_TRACE_OUTPUT"},
			Hidden:  shouldHide,
		}),
	}
}

func configureProxyDNSFlags(shouldHide bool) []cli.Flag {
	return []cli.Flag{
		altsrc.NewBoolFlag(&cli.BoolFlag{
			Name:    "proxy-dns",
			Usage:   "Run a DNS over HTTPS proxy server.",
			EnvVars: []string{"TUNNEL_DNS"},
			Hidden:  shouldHide,
		}),
		altsrc.NewIntFlag(&cli.IntFlag{
			Name:    "proxy-dns-port",
			Value:   53,
			Usage:   "Listen on given port for the DNS over HTTPS proxy server.",
			EnvVars: []string{"TUNNEL_DNS_PORT"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringFlag(&cli.StringFlag{
			Name:    "proxy-dns-address",
			Usage:   "Listen address for the DNS over HTTPS proxy server.",
			Value:   "localhost",
			EnvVars: []string{"TUNNEL_DNS_ADDRESS"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
			Name:    "proxy-dns-upstream",
			Usage:   "Upstream endpoint URL, you can specify multiple endpoints for redundancy.",
			Value:   cli.NewStringSlice("https://1.1.1.1/dns-query", "https://1.0.0.1/dns-query"),
			EnvVars: []string{"TUNNEL_DNS_UPSTREAM"},
			Hidden:  shouldHide,
		}),
		altsrc.NewStringSliceFlag(&cli.StringSliceFlag{
			Name:    "proxy-dns-bootstrap",
			Usage:   "bootstrap endpoint URL, you can specify multiple endpoints for redundancy.",
			Value:   cli.NewStringSlice("https://162.159.36.1/dns-query", "https://162.159.46.1/dns-query", "https://[2606:4700:4700::1111]/dns-query", "https://[2606:4700:4700::1001]/dns-query"),
			EnvVars: []string{"TUNNEL_DNS_BOOTSTRAP"},
			Hidden:  shouldHide,
		}),
	}
}

func stdinControl(reconnectCh chan origin.ReconnectSignal, logger logger.Service) {
	for {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			command := scanner.Text()
			parts := strings.SplitN(command, " ", 2)

			switch parts[0] {
			case "":
				break
			case "reconnect":
				var reconnect origin.ReconnectSignal
				if len(parts) > 1 {
					var err error
					if reconnect.Delay, err = time.ParseDuration(parts[1]); err != nil {
						logger.Error(err.Error())
						continue
					}
				}
				logger.Infof("Sending reconnect signal %+v", reconnect)
				reconnectCh <- reconnect
			default:
				logger.Infof("Unknown command: %s", command)
				fallthrough
			case "help":
				logger.Info(`Supported command:
reconnect [delay]
- restarts one randomly chosen connection with optional delay before reconnect`)
			}
		}
	}
}

func buildValidateCommand() *cli.Command {
	return &cli.Command{
		Name:        "validate",
		Action:      cliutil.ErrorHandler(ValidateCommand),
		Usage:       "Validate the ingress configuration ",
		UsageText:   "cloudflared tunnel [--config FILEPATH] ingress validate",
		Description: "Validates the configuration file, ensuring your ingress rules are OK.",
	}
}

func buildRuleCommand() *cli.Command {
	return &cli.Command{
		Name:      "rule",
		Action:    cliutil.ErrorHandler(RuleCommand),
		Usage:     "Check which ingress rule matches a given request URL",
		UsageText: "cloudflared tunnel [--config FILEPATH] ingress rule URL",
		ArgsUsage: "URL",
		Description: "Check which ingress rule matches a given request URL. " +
			"Ingress rules match a request's hostname and path. Hostname is " +
			"optional and is either a full hostname like `www.example.com` or a " +
			"hostname with a `*` for its subdomains, e.g. `*.example.com`. Path " +
			"is optional and matches a regular expression, like `/[a-zA-Z0-9_]+.html`",
	}
}

// Validates the ingress rules in the cloudflared config file
func ValidateCommand(c *cli.Context) error {
	_, err := config.ReadRules(c)
	if err != nil {
		return errors.Wrap(err, "Validation failed")
	}
	if c.IsSet("url") {
		return ingress.ErrURLIncompatibleWithIngress
	}
	fmt.Println("OK")
	return nil
}

// Checks which ingress rule matches the given URL.
func RuleCommand(c *cli.Context) error {
	rules, err := config.ReadRules(c)
	if err != nil {
		return err
	}
	requestArg := c.Args().First()
	if requestArg == "" {
		return errors.New("cloudflared tunnel rule expects a single argument, the URL to test")
	}
	requestURL, err := url.Parse(requestArg)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL", requestArg)
	}
	if requestURL.Hostname() == "" && requestURL.Scheme == "" {
		return fmt.Errorf("%s doesn't have a hostname, consider adding a scheme", requestArg)
	}
	if requestURL.Hostname() == "" {
		return fmt.Errorf("%s doesn't have a hostname", requestArg)
	}
	return ingress.RuleCommand(rules, requestURL)
}
