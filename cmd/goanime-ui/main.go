// Command goanime-ui serves GoAnime's browser interface on localhost.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/alvarorichard/Goanime/internal/util"
	"github.com/alvarorichard/Goanime/internal/webui"
)

func main() {
	port := flag.Int("port", 7777, "porta do servidor local")
	noOpen := flag.Bool("no-open", false, "não abrir o navegador automaticamente")
	flag.Parse()

	home, err := os.UserHomeDir()
	if err != nil {
		fail(err)
	}
	store, err := webui.OpenStore(filepath.Join(home, ".local", "goanime", "ui", "state.json"))
	if err != nil {
		fail(fmt.Errorf("lendo favoritos/histórico: %w", err))
	}

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fail(fmt.Errorf("a porta %d já está em uso — o GoAnime UI já está aberto? (%w)", *port, err))
	}
	appURL := "http://" + addr
	fmt.Printf("GoAnime UI rodando em %s (Ctrl+C para sair)\n", appURL)

	if !*noOpen {
		_ = exec.Command("xdg-open", appURL).Start()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		util.RunCleanup()
		os.Exit(0)
	}()

	srv := &http.Server{Handler: webui.NewServer(store, util.DefaultDownloadDir()).Handler()}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "goanime-ui:", err)
	os.Exit(1)
}
