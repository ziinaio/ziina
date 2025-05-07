package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/giancarlosio/gorainbow"
	"github.com/gliderlabs/ssh"
	"github.com/urfave/cli/v2"
	sshcrypto "golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
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
				Name:    "listen",
				Aliases: []string{"l"},
				Usage:   "Listen on this port.",
				Value:   ":2222",
			},
			&cli.StringFlag{
				Name:     "server",
				Aliases:  []string{"s"},
				Usage:    "The SSH server to use as endpoint.",
				Required: true,
			},
			&cli.StringFlag{
				Name:    "user",
				Aliases: []string{"u"},
				Usage:   "Username for SSH authentication.",
			},
			&cli.StringFlag{
				Name:    "key",
				Aliases: []string{"k"},
				Usage:   "Path to the private key for SSH authentication.",
				Value:   os.Getenv("HOME") + "/.ssh/id_rsa",
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

			var u *user.User
			if ctx.String("user") == "" {
				var err error
				u, err = user.Current()
				if err != nil {
					return err
				}
			}

			// Start the SSH server
			go func() {
				if err := runServer(ctx.String("listen")); err != nil {
					log.Fatalf("SSH server error: %v", err)
				}
			}()

			go func() {
				if err := runReverseTunnel(ctx.String("server"), u.Username, port); err != nil {
					log.Fatalf("SSH remote port-forwarding tunnel terminated: %s\n", err)
				}
			}()

			// Start the reverse SSH tunnel
			return runZellij(ctx.String("server"), u.Username, port)
			// return runReverseTunnel(ctx.String("server"), u.Username, port)
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
	keyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return fmt.Errorf("could not read SSH host key: %w", err)
	}

	private, err := sshcrypto.ParsePrivateKey(keyBytes)
	if err != nil {
		return fmt.Errorf("could not parse SSH host key: %w", err)
	}

	server.AddHostKey(private)

	log.Printf("Starting Ziina server on %s...\n", listenAddr)
	return server.ListenAndServe()
}

func runReverseTunnel(remoteHost, user string, port int) error {
	log.Println("Starting SSH reverse port-forwarding...")

	// Connect to the running SSH agent
	// This will look for an environment variable SSH_AUTH_SOCK to determine where the agent socket is
	sshAgentSocket := os.Getenv("SSH_AUTH_SOCK")
	if sshAgentSocket == "" {
		log.Fatalf("SSH agent not found. Please ensure SSH agent is running and SSH_AUTH_SOCK is set.")
	}

	// Open the agent socket
	agentConn, err := net.Dial("unix", sshAgentSocket)
	if err != nil {
		log.Fatalf("Failed to connect to SSH agent: %s", err)
	}
	defer agentConn.Close()

	// Create a new agent client
	agentClient := sshagent.NewClient(agentConn)

	// SSH client configuration
	config := &sshcrypto.ClientConfig{
		User: user, // Replace with your SSH username
		Auth: []sshcrypto.AuthMethod{
			// Use the SSH agent to retrieve keys for authentication
			sshcrypto.PublicKeysCallback(agentClient.Signers),
		},
		HostKeyCallback: sshcrypto.InsecureIgnoreHostKey(), // For development, replace with proper verification in production
	}

	client, err := sshcrypto.Dial("tcp", fmt.Sprintf("%s:22", remoteHost), config)
	if err != nil {
		return fmt.Errorf("failed to dial SSH server: %v", err)
	}

	// Request remote port forwarding
	listener, err := client.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return fmt.Errorf("failed to set up remote port forwarding: %v", err)
	}

	log.Printf("Remote port forwarding established: %s:%d -> localhost:%d", remoteHost, port, port)

	// Handle incoming connections
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("Listener accept error: %v", err)
				continue
			}

			// Connect to the local SSH server
			localConn, err := net.Dial("tcp", fmt.Sprintf("localhost:%d", port))
			if err != nil {
				log.Printf("Failed to connect to local service: %v", err)
				conn.Close()
				continue
			}

			// Start bidirectional copy
			go func() {
				defer conn.Close()
				defer localConn.Close()
				go io.Copy(localConn, conn)
				io.Copy(conn, localConn)
			}()
		}
	}()

	// Wait for interrupt signal to gracefully shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Println("Shutting down...")

	return nil
}

func runZellij(server, user string, port int) error {
	time.Sleep(time.Second)

	// Connect to SSH agent
	sshAgentSock := os.Getenv("SSH_AUTH_SOCK")
	if sshAgentSock == "" {
		return fmt.Errorf("SSH_AUTH_SOCK not set")
	}
	agentConn, err := net.Dial("unix", sshAgentSock)
	if err != nil {
		return fmt.Errorf("failed to connect to SSH agent: %w", err)
	}
	defer agentConn.Close()
	ag := sshagent.NewClient(agentConn)

	// SSH config
	config := &sshcrypto.ClientConfig{
		User: user,
		Auth: []sshcrypto.AuthMethod{
			sshcrypto.PublicKeysCallback(ag.Signers),
		},
		HostKeyCallback: sshcrypto.InsecureIgnoreHostKey(), // Don't use this in production
	}

	// Connect
	addr := fmt.Sprintf("%s:%d", server, port)
	client, err := sshcrypto.Dial("tcp", addr, config)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	defer client.Close()

	// Create session
	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Save current terminal state
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("failed to set terminal raw mode: %w", err)
	}
	defer term.Restore(fd, oldState)

	// Handle Ctrl+C gracefully
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		term.Restore(fd, oldState)
		os.Exit(0)
	}()

	// Request PTY
	termType := os.Getenv("TERM")
	if termType == "" {
		termType = "xterm-256color"
	}
	width, height, err := term.GetSize(fd)
	if err != nil {
		width, height = 80, 24 // fallback
	}
	err = session.RequestPty(termType, height, width, sshcrypto.TerminalModes{
		sshcrypto.ECHO: 1,
	})
	if err != nil {
		return fmt.Errorf("request for PTY failed: %w", err)
	}

	// Set I/O
	session.Stdin = os.Stdin
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	// Start Zellij
	if err := session.Start("zellij attach"); err != nil {
		return fmt.Errorf("failed to start zellij: %w", err)
	}

	// Wait for session to end
	if err := session.Wait(); err != nil {
		return fmt.Errorf("zellij session ended with error: %w", err)
	}

	return nil
}
