package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/tareksalem/falak/internal/node"
)

func main() {
	var (
		clusterName = flag.String("cluster", "default", "Name of the cluster")
		outputDir   = flag.String("output", "./cluster-ca", "Output directory for CA files")
		exportOnly  = flag.String("export", "", "Export CA certificate to specified file (for node distribution)")
		verify      = flag.Bool("verify", false, "Verify existing Root CA")
	)

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Falak Cluster CA Generator\n\n")
		fmt.Fprintf(os.Stderr, "This utility generates and manages the Root CA certificate for a Falak cluster.\n")
		fmt.Fprintf(os.Stderr, "The Root CA must be generated ONCE per cluster and distributed to all nodes.\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  Generate new Root CA:  %s -cluster=<name> -output=<dir>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  Export CA cert only:   %s -output=<dir> -export=<file>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  Verify existing CA:    %s -output=<dir> -verify\n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	// Create CA manager
	caManager := node.NewClusterCAManager(*outputDir)

	// Handle different operations
	if *verify {
		// Verify existing CA
		if err := verifyCA(caManager); err != nil {
			log.Fatalf("❌ Verification failed: %v", err)
		}
		return
	}

	if *exportOnly != "" {
		// Export CA certificate for distribution
		if err := exportCA(caManager, *exportOnly); err != nil {
			log.Fatalf("❌ Export failed: %v", err)
		}
		return
	}

	// Generate new Root CA
	if err := generateCA(caManager, *clusterName, *outputDir); err != nil {
		log.Fatalf("❌ CA generation failed: %v", err)
	}
}

func generateCA(caManager *node.ClusterCAManager, clusterName, outputDir string) error {
	// Check if CA already exists
	caPath := filepath.Join(outputDir, "root-ca.crt")
	if _, err := os.Stat(caPath); err == nil {
		return fmt.Errorf("Root CA already exists at %s. Delete it first if you want to regenerate", caPath)
	}

	fmt.Println("🔐 Generating Root CA for Falak Cluster")
	fmt.Printf("   Cluster Name: %s\n", clusterName)
	fmt.Printf("   Output Directory: %s\n", outputDir)
	fmt.Println()

	// Generate the Root CA
	if err := caManager.GenerateRootCA(clusterName); err != nil {
		return fmt.Errorf("failed to generate Root CA: %w", err)
	}

	fmt.Println("\n✅ Root CA generated successfully!")
	fmt.Println("\n📋 Next Steps:")
	fmt.Println("1. Keep root-ca.key SECURE - it's needed to sign node certificates")
	fmt.Println("2. Distribute root-ca.crt to all nodes in the cluster")
	fmt.Println("3. Each node needs:")
	fmt.Printf("   - The root-ca.crt file (copy to node's cert directory)\n")
	fmt.Printf("   - Its own certificate signed by this CA (will be auto-generated)\n")
	fmt.Println("\n🔐 Files created:")
	fmt.Printf("   %s/root-ca.crt - Root CA certificate (distribute to nodes)\n", outputDir)
	fmt.Printf("   %s/root-ca.key - Root CA private key (KEEP SECURE!)\n", outputDir)

	// Also export the CA certificate for easy distribution
	exportPath := filepath.Join(outputDir, "root-ca-distribute.crt")
	if err := caManager.ExportRootCACertificate(exportPath); err != nil {
		log.Printf("⚠️  Warning: Failed to create distribution copy: %v", err)
	}

	return nil
}

func exportCA(caManager *node.ClusterCAManager, exportPath string) error {
	fmt.Println("📤 Exporting Root CA certificate for distribution")

	// Load existing CA
	if err := caManager.LoadRootCA(); err != nil {
		return fmt.Errorf("failed to load Root CA: %w", err)
	}

	// Export certificate
	if err := caManager.ExportRootCACertificate(exportPath); err != nil {
		return fmt.Errorf("failed to export certificate: %w", err)
	}

	fmt.Printf("\n✅ Root CA certificate exported to: %s\n", exportPath)
	fmt.Println("   Distribute this file to all nodes in the cluster")

	return nil
}

func verifyCA(caManager *node.ClusterCAManager) error {
	fmt.Println("🔍 Verifying Root CA")

	// Load existing CA
	if err := caManager.LoadRootCA(); err != nil {
		return fmt.Errorf("failed to load Root CA: %w", err)
	}

	ca := caManager.GetRootCA()
	if ca == nil {
		return fmt.Errorf("no Root CA loaded")
	}

	fmt.Println("\n✅ Root CA verified successfully!")
	fmt.Printf("   Subject: %s\n", ca.Subject)
	fmt.Printf("   Issuer: %s\n", ca.Issuer)
	fmt.Printf("   Serial: %s\n", ca.SerialNumber)
	fmt.Printf("   Valid from: %s\n", ca.NotBefore)
	fmt.Printf("   Valid until: %s\n", ca.NotAfter)
	fmt.Printf("   Is CA: %v\n", ca.IsCA)

	return nil
}