package cmds

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof" // nolint: golint
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ipfs/go-ipfs-cmdkit"
	"github.com/ipfs/go-ipfs-cmds"
	cmdhttp "github.com/ipfs/go-ipfs-cmds/http"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multiaddr-net"
	"github.com/pkg/errors"

	"github.com/filecoin-project/go-filecoin/tools/network-randomizer/config"
	"github.com/filecoin-project/go-filecoin/tools/network-randomizer/node"
	"github.com/filecoin-project/go-filecoin/tools/network-randomizer/repo"
)

// exposed here, to be available during testing
var sigCh = make(chan os.Signal, 1)

var daemonCmd = &cmds.Command{
	Helptext: cmdkit.HelpText{
		Tagline: "Start a long-running daemon process",
	},
	Run: func(req *cmds.Request, re cmds.ResponseEmitter, env cmds.Environment) error {
		return daemonRun(req, re, env)
	},
	Encoders: cmds.EncoderMap{
		cmds.Text: cmds.Encoders[cmds.Text],
	},
}

func daemonRun(req *cmds.Request, re cmds.ResponseEmitter, env cmds.Environment) error {
	// third precedence is config file.
	rep, err := getRepo(req)
	if err != nil {
		return err
	}

	// second highest precedence is env vars.
	if envAPI := os.Getenv(env_fcnr_api); envAPI != "" {
		rep.Config().API.Address = envAPI
	}

	// highest precedence is cmd line flag.
	if flagAPI, ok := req.Options[OptionAPI].(string); ok && flagAPI != "" {
		rep.Config().API.Address = flagAPI
	}

	opts, err := node.OptionsFromRepo(rep)
	if err != nil {
		return err
	}

	fcn, err := node.New(req.Context, opts...)
	if err != nil {
		return err
	}

	re.Emit(fmt.Sprintf("My peer ID is %s\n", fcn.Host().ID().Pretty())) // nolint: errcheck
	for _, a := range fcn.Host().Addrs() {
		re.Emit(fmt.Sprintf("Swarm listening on: %s\n", a)) // nolint: errcheck
	}

	return runAPIAndWait(req.Context, fcn, rep.Config(), req)
}

func getRepo(req *cmds.Request) (repo.Repo, error) {
	/*
		repoDir, _ := req.Options[OptionRepoDir].(string)
		repoDir, err := paths.GetRepoPath(repoDir)
		if err != nil {
			return nil, err
		}
	*/
	repoDir := "~/.fcnr"
	return repo.OpenFSRepo(repoDir)
}

func runAPIAndWait(ctx context.Context, nd *node.Node, config *config.Config, req *cmds.Request) error {
	if err := nd.Start(ctx); err != nil {
		return err
	}
	defer nd.Stop(ctx)

	servenv := &Env{
		ctx: context.Background(),
	}

	cfg := cmdhttp.NewServerConfig()
	cfg.APIPath = APIPrefix

	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	maddr, err := ma.NewMultiaddr(config.API.Address)
	if err != nil {
		return err
	}

	// For the case when /ip4/127.0.0.1/tcp/0 is passed,
	// we want to fetch the new multiaddr from the listener, as it may (should)
	// have resolved to some other value. i.e. resolve port zero to real value.
	apiLis, err := manet.Listen(maddr)
	if err != nil {
		return err
	}
	config.API.Address = apiLis.Multiaddr().String()

	handler := http.NewServeMux()
	handler.Handle("/debug/pprof/", http.DefaultServeMux)
	handler.Handle(APIPrefix+"/", cmdhttp.NewHandler(servenv, rootCmdDaemon, cfg))

	apiserv := http.Server{
		Handler: handler,
	}

	go func() {
		err := apiserv.Serve(manet.NetListener(apiLis))
		if err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()

	// write our api address to file
	if err := nd.Repo.SetAPIAddr(config.API.Address); err != nil {
		return errors.Wrap(err, "Could not save API address to repo")
	}

	signal := <-sigCh
	fmt.Printf("Got %s, shutting down...\n", signal)

	// allow 5 seconds for clean shutdown. Ideally it would never take this long.
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()

	if err := apiserv.Shutdown(ctx); err != nil {
		fmt.Println("failed to shut down api server:", err)
	}

	return nil
}