// Command inject is a local tool that reconstructs Kiro IDE / kiro-cli
// credential stores from a Kiro-Go account export, so you can switch the
// logged-in account on your machine without a fresh browser login.
//
// It must run on the same machine as the Kiro IDE / kiro-cli (the Kiro-Go
// server usually runs in Docker and cannot reach the host filesystem). It
// serves a small web UI on 127.0.0.1 and, unless --no-open is set, opens it in
// the default browser.
//
// Flags:
//
//	--port       port to listen on (default 7878)
//	--no-open    do not auto-open the browser
//	--cache-dir  override the Kiro IDE cache dir (for testing without touching ~/.aws)
//	--cli-db     override the kiro-cli sqlite path (for testing)
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"kiro-go/injector"
)

func main() {
	port := flag.Int("port", 7878, "port to listen on (127.0.0.1)")
	noOpen := flag.Bool("no-open", false, "do not auto-open the browser")
	cacheDir := flag.String("cache-dir", "", "override Kiro IDE cache dir (testing)")
	cliDB := flag.String("cli-db", "", "override kiro-cli sqlite path (testing)")
	flag.Parse()

	srv := injector.NewServer()
	if *cacheDir != "" || *cliDB != "" {
		srv.SetDryRunOverrides(*cacheDir, *cliDB)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("cannot bind %s: %v", addr, err)
	}

	url := fmt.Sprintf("http://%s/", addr)
	log.Printf("Kiro account injector running at %s", url)
	if !*noOpen {
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = openBrowser(url)
		}()
	}

	httpSrv := &http.Server{Handler: srv.Handler()}
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// openBrowser opens url in the platform default browser. Best-effort.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("cmd", "/C", "start", "", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
