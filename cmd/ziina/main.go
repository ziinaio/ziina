package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"github.com/giancarlosio/gorainbow"
	"github.com/gliderlabs/ssh"
	"github.com/urfave/cli/v2"
	sshcrypto "golang.org/x/crypto/ssh"
)

const banner = `
███████╗██╗██╗███╗   ██╗ █████╗ 
╚══███╔╝██║██║████╗  ██║██╔══██╗
  ███╔╝ ██║██║██╔██╗ ██║███████║
 ███╔╝  ██║██║██║╚██╗██║██╔══██║
███████╗██║██║██║ ╚████║██║  ██║
╚══════╝╚═╝╚═╝╚═╝  ╚═══╝╚═╝  ╚═╝
`
func main() {
	app := &cli.App{
		Name:  "ziina",
		Usage: "💻 📤 👥 Instant terminal sharing; using Zellij." + "\n" + gorainbow.Rainbow(banner),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name: "listen",
				Aliases: []string{"l"},
				Usage: "Listen on this port.",
				Value: ":2222",
			},
			&cli.StringFlag{
				Name: "server",
				Aliases: []string{"s"},
				Usage: "The SSHserver to use as endpoint.",
				Required: true,
			},
		},
		Action: func(ctx *cli.Context) error {
			parts := strings.Split(ctx.String("listen"), ":")
			if len(parts) != 2 {
				return fmt.Errorf("invalid listen address: %s", ctx.String("listen"))
			}
			portStr := parts[1]
			port, err := strconv.Atoi(portStr)
			if err != nil {
				return err
			}

			go run(ctx.Context, ctx.String("server"), port, port)
			return runServer(ctx.String("listen"))
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

var (
	userSessions = make(map[string][]ssh.Session)
	mu           sync.Mutex
)

func runServer(listenAddr string) error {
	// Define the SSH server
	server := &ssh.Server{
		Addr: listenAddr,
		Handler: func(s ssh.Session) {
			username := s.User()

			// Add session to the user pool
			mu.Lock()
			userSessions[username] = append(userSessions[username], s)
			mu.Unlock()

			cmd := exec.Command("zellij", "-l", "compact", "attach", "--create", username)

			ptyReq, winCh, isPty := s.Pty()
			if !isPty {
				io.WriteString(s, "No PTY requested. Zellij requires a PTY.\n")
				s.Exit(1)
				return
			}

			// Set TERM environment variable
			cmd.Env = append(cmd.Env, fmt.Sprintf("TERM=%s", ptyReq.Term))

			// Start Zellij in a new PTY
			ptmx, err := pty.Start(cmd)
			if err != nil {
				log.Printf("Failed to start PTY: %v", err)
				s.Exit(1)
				return
			}
			defer ptmx.Close()

			// Handle window resize
			go func() {
				for win := range winCh {
					pty.Setsize(ptmx, &pty.Winsize{
						Cols: uint16(win.Width),
						Rows: uint16(win.Height),
					})
				}
			}()

			// Connect session input/output to the PTY
			go io.Copy(ptmx, s)
			io.Copy(s, ptmx) // blocks until Zellij exits
		},
	}

	// Load the host key from a file using golang.org/x/crypto/ssh to parse
	privateKeyPath := "ssh_host_rsa_key" // Adjust the path to your host private key
	hostKey, err := os.ReadFile(privateKeyPath)
	if err == nil {
		// Parse the host key using the sshcrypto package from golang.org/x/crypto/ssh
		parsedHostKey, err := sshcrypto.ParsePrivateKey(hostKey)
		if err == nil {
			// Add the host key to the server
			server.AddHostKey(parsedHostKey)
		}
	}

	log.Printf("Starting Ziina server on %s...\n", listenAddr)
	return  server.ListenAndServe()
}

func run(ctx context.Context, remoteHost string, remotePort, localPort int) {
	log.Println("Starting SSH remote port-forwarding...")
	cmd := exec.Command(
		"ssh",
		"-N",
		"-R",
		fmt.Sprintf("%d:localhost:%d", remotePort, localPort),
		remoteHost,
	)

	go func() {
			<-ctx.Done()
			log.Println("Context canceled, sending SIGTERM to ssh")
			if cmd.Process != nil {
					_ = cmd.Process.Signal(syscall.SIGTERM)
			}
	}()

	if err := cmd.Run(); err != nil {
			log.Printf("SSH exited: %v", err)
	}
}

