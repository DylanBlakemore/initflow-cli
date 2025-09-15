package cmd

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"

	"github.com/DylanBlakemore/initflow-cli/internal/client"
	"github.com/DylanBlakemore/initflow-cli/internal/encoding"
	"github.com/DylanBlakemore/initflow-cli/internal/storage"
)

var workspaceCmd = &cobra.Command{
	Use:   "workspace",
	Short: "Manage workspaces and workspace keys",
	Long:  `Manage workspaces and workspace keys for secure secret storage.`,
}

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all workspaces",
	Long:  `List all workspaces and their key initialization status.`,
	RunE:  runWorkspaceList,
}

var workspaceInitCmd = &cobra.Command{
	Use:   "init <workspace-slug>",
	Short: "Initialize workspace key",
	Long:  `Initialize a new workspace key for secure secret storage.`,
	Args:  cobra.ExactArgs(1),
	RunE:  runWorkspaceInit,
}

func init() {
	rootCmd.AddCommand(workspaceCmd)
	workspaceCmd.AddCommand(workspaceListCmd)
	workspaceCmd.AddCommand(workspaceInitCmd)
}

func runWorkspaceList(cmd *cobra.Command, args []string) error {
	fmt.Println("🔍 Fetching workspaces...")

	store := storage.New()
	if !store.HasDeviceID() {
		return fmt.Errorf("❌ Device not registered. Please run 'initflow device register <name>' first")
	}

	c := client.New()
	workspaces, err := c.ListWorkspaces()
	if err != nil {
		return fmt.Errorf("❌ Failed to fetch workspaces: %w", err)
	}

	if len(workspaces) == 0 {
		fmt.Println("No workspaces found. Create one at https://app.initflow.com")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 1, ' ', tabwriter.Debug)
	fmt.Fprintln(w, "Name\tSlug\tKey Initialized\tRole")
	fmt.Fprintln(w, "────\t────\t───────────────\t────")

	for _, workspace := range workspaces {
		keyStatus := "❌ No"
		if workspace.KeyInitialized {
			keyStatus = "✅ Yes"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			workspace.Name,
			workspace.Slug,
			keyStatus,
			workspace.Role)
	}
	_ = w.Flush()

	hasUninitialized := false
	for _, workspace := range workspaces {
		if !workspace.KeyInitialized {
			hasUninitialized = true
			break
		}
	}

	if hasUninitialized {
		fmt.Println("\n💡 Initialize keys for workspaces marked \"No\" using:")
		fmt.Println("   initflow workspace init <workspace-slug>")
	}

	return nil
}

func runWorkspaceInit(cmd *cobra.Command, args []string) error {
	workspaceSlug := args[0]

	fmt.Printf("🔐 Initializing workspace key for \"%s\"...\n", workspaceSlug)

	store := storage.New()
	if !store.HasDeviceID() {
		return fmt.Errorf("❌ Device not registered. Please run 'initflow device register <name>' first")
	}

	if store.HasWorkspaceKey(workspaceSlug) {
		fmt.Println("ℹ️ Workspace key already exists locally")
		return nil
	}

	c := client.New()
	workspace, err := c.GetWorkspaceBySlug(workspaceSlug)
	if err != nil {
		return fmt.Errorf("❌ Failed to get workspace info: %w", err)
	}

	if workspace.KeyInitialized {
		return fmt.Errorf("ℹ️ Workspace key already initialized")
	}

	fmt.Println("⚡ Generating secure 256-bit workspace key...")
	workspaceKey := make([]byte, encoding.WorkspaceKeySize)
	if _, err := rand.Read(workspaceKey); err != nil {
		return fmt.Errorf("❌ Failed to generate workspace key: %w", err)
	}

	fmt.Println("🔒 Encrypting with your device's X25519 key...")
	wrappedKey, err := wrapWorkspaceKey(workspaceKey, store)
	if err != nil {
		return fmt.Errorf("❌ Failed to encrypt workspace key: %w", err)
	}

	fmt.Println("📡 Uploading encrypted key to server...")
	if err := c.InitializeWorkspaceKey(workspace.ID, wrappedKey); err != nil {
		return fmt.Errorf("❌ Failed to initialize workspace key: %w", err)
	}

	if err := store.StoreWorkspaceKey(workspaceSlug, workspaceKey); err != nil {
		return fmt.Errorf("❌ Failed to store workspace key locally: %w", err)
	}

	fmt.Println("✅ Workspace key initialized successfully!")
	fmt.Println("🎯 You can now store and retrieve secrets in this workspace.")
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  • Add secrets: initflow secrets add API_KEY=your-secret")
	fmt.Println("  • List secrets: initflow secrets list")
	fmt.Println("  • Invite devices: initflow workspace invite-device")

	return nil
}

func wrapWorkspaceKey(workspaceKey []byte, store *storage.Storage) ([]byte, error) {
	encryptionPrivateKey, err := store.GetEncryptionPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to get encryption private key: %w", err)
	}

	ephemeralPrivate := make([]byte, encoding.X25519PrivateKeySize)
	if _, err := rand.Read(ephemeralPrivate); err != nil {
		return nil, fmt.Errorf("failed to generate ephemeral private key: %w", err)
	}

	ephemeralPublic, err := curve25519.X25519(ephemeralPrivate, curve25519.Basepoint)
	if err != nil {
		return nil, fmt.Errorf("failed to generate ephemeral public key: %w", err)
	}

	sharedSecret, err := curve25519.X25519(encryptionPrivateKey, ephemeralPublic)
	if err != nil {
		return nil, fmt.Errorf("failed to compute shared secret: %w", err)
	}

	hkdf := hkdf.New(sha256.New, sharedSecret, []byte("initflow.wrap"), []byte("workspace"))
	encryptionKey := make([]byte, encoding.WorkspaceKeySize)
	if _, err := hkdf.Read(encryptionKey); err != nil {
		return nil, fmt.Errorf("failed to derive encryption key: %w", err)
	}

	cipher, err := chacha20poly1305.New(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	nonce := make([]byte, encoding.ChaCha20NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := cipher.Seal(nil, nonce, workspaceKey, nil) // #nosec G407 - nonce is randomly generated above

	wrapped := make([]byte, 0, 32+12+len(ciphertext))
	wrapped = append(wrapped, ephemeralPublic...)
	wrapped = append(wrapped, nonce...)
	wrapped = append(wrapped, ciphertext...)

	return wrapped, nil
}
