// build 是 iperf3-tool 项目自己的跨平台发布程序。
//
// 它故意只依赖 Go 标准库，避免为了“选择要构建的平台”再依赖 Bash、PowerShell
// 或某个操作系统专有的压缩命令。开发者既可以通过 Taskfile 调用它，也可以直接
// 执行 `go run ./cmd/build package`。
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const applicationName = "iperf3-tool"

// target 表示一个 Go 原生二进制的目标操作系统和处理器架构。
type target struct {
	OS   string
	Arch string
}

func (t target) String() string { return t.OS + "/" + t.Arch }

// supportedTargets 是本项目对外提供的发布范围。这里没有直接开放 `go tool dist
// list` 的全部组合，因为其中包含 WASM、移动端和小众系统，而本项目需要启动本机
// iperf3 进程，这些目标即使能编译，也不代表具备可用的运行环境。
var supportedTargets = []target{
	{OS: "darwin", Arch: "amd64"},
	{OS: "darwin", Arch: "arm64"},
	{OS: "linux", Arch: "amd64"},
	{OS: "linux", Arch: "arm64"},
	{OS: "windows", Arch: "amd64"},
	{OS: "windows", Arch: "arm64"},
}

var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,63}$`)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	root, err := findProjectRoot()
	if err != nil {
		fail(err)
	}

	switch os.Args[1] {
	case "list":
		listTargets()
	case "package":
		if err := packageCommand(root, os.Args[2:]); err != nil {
			fail(err)
		}
	case "clean":
		if err := cleanCommand(root); err != nil {
			fail(err)
		}
	case "help", "-h", "--help":
		printUsage()
	default:
		_, err2 := fmt.Fprintf(os.Stderr, "未知子命令 %q\n\n", os.Args[1])
		if err2 != nil {
			return
		}
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Println(`iperf3-tool 构建工具

用法：
  go run ./cmd/build list
  go run ./cmd/build package [-targets current] [-version dev]
  go run ./cmd/build clean

示例：
  go run ./cmd/build package -targets windows/amd64 -version v1.0.0
  go run ./cmd/build package -targets windows/amd64,linux/arm64 -version v1.0.0`)
}

func listTargets() {
	current := target{OS: runtime.GOOS, Arch: runtime.GOARCH}
	fmt.Println("可选目标：")
	for _, item := range supportedTargets {
		mark := ""
		if item == current {
			mark = "（当前平台）"
		}
		fmt.Printf("  %-16s %s\n", item, mark)
	}
	fmt.Println("\n不指定 -targets 时等同于 current，只构建当前平台。")
}

func packageCommand(root string, arguments []string) error {
	flags := flag.NewFlagSet("package", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	targetText := flags.String("targets", "current", "逗号分隔的目标，例如 windows/amd64,linux/arm64")
	version := flags.String("version", "dev", "写入文件名和程序的版本号")
	skipFrontend := flags.Bool("skip-frontend", false, "跳过 npm ci 和前端构建，使用现有嵌入资源")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("无法识别的额外参数：%s", strings.Join(flags.Args(), " "))
	}
	if !versionPattern.MatchString(*version) {
		return errors.New("版本号只能包含字母、数字、点、下划线和连字符，且长度不能超过 64")
	}

	targets, err := parseTargets(*targetText)
	if err != nil {
		return err
	}

	// 前端产物会被 go:embed 编译进最终二进制，因此必须先构建前端，
	// 再编译 Go 程序。npm ci 按 package-lock.json 安装依赖，保证发布构建可重复。
	if !*skipFrontend {
		if err := run(root, nil, "npm", "--prefix", "webui", "ci"); err != nil {
			return fmt.Errorf("安装前端依赖失败：%w", err)
		}
		if err := run(root, nil, "npm", "--prefix", "webui", "run", "build"); err != nil {
			return fmt.Errorf("构建前端失败：%w", err)
		}
	}
	outputDir := filepath.Join(root, "dist")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败：%w", err)
	}
	stagingDir, err := os.MkdirTemp(outputDir, ".build-")
	if err != nil {
		return fmt.Errorf("创建临时构建目录失败：%w", err)
	}
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {

		}
	}(stagingDir)

	for _, item := range targets {
		if err := buildTarget(root, outputDir, stagingDir, *version, item); err != nil {
			return err
		}
	}
	if err := writeChecksums(outputDir); err != nil {
		return err
	}

	fmt.Printf("\n打包完成：%s\n", outputDir)
	fmt.Println("校验文件：dist/checksums.txt")
	return nil
}

func parseTargets(value string) ([]target, error) {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(parts) == 0 {
		return nil, errors.New("至少需要选择一个构建目标")
	}

	allowed := make(map[string]target, len(supportedTargets))
	for _, item := range supportedTargets {
		allowed[item.String()] = item
	}

	seen := make(map[string]bool)
	result := make([]target, 0, len(parts))
	for _, part := range parts {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "current" {
			name = runtime.GOOS + "/" + runtime.GOARCH
		}
		item, ok := allowed[name]
		if !ok {
			return nil, fmt.Errorf("不支持目标 %q；请运行 `go run ./cmd/build list` 查看可选项", part)
		}
		if !seen[name] {
			seen[name] = true
			result = append(result, item)
		}
	}
	return result, nil
}

func buildTarget(root, outputDir, stagingDir, version string, item target) error {
	extension := ""
	archiveExtension := ".tar.gz"
	if item.OS == "windows" {
		extension = ".exe"
		archiveExtension = ".zip"
	}
	binaryName := applicationName + extension
	binaryPath := filepath.Join(stagingDir, item.OS+"_"+item.Arch, binaryName)
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		return fmt.Errorf("创建 %s 临时目录失败：%w", item, err)
	}

	fmt.Printf("\n==> 构建 %s\n", item)
	buildEnvironment := map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        item.OS,
		"GOARCH":      item.Arch,
	}
	ldflags := "-s -w -X main.version=" + version
	if err := run(root, buildEnvironment, "go", "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", binaryPath, "."); err != nil {
		return fmt.Errorf("构建 %s 失败：%w", item, err)
	}

	archiveName := fmt.Sprintf("%s_%s_%s_%s%s", applicationName, version, item.OS, item.Arch, archiveExtension)
	archivePath := filepath.Join(outputDir, archiveName)
	if err := archiveBinary(binaryPath, binaryName, archivePath, item.OS == "windows"); err != nil {
		return fmt.Errorf("压缩 %s 失败：%w", item, err)
	}
	fmt.Printf("    已生成 dist/%s\n", archiveName)
	return nil
}

func archiveBinary(binaryPath, binaryName, archivePath string, windows bool) error {
	if windows {
		return writeZip(binaryPath, binaryName, archivePath)
	}
	return writeTarGzip(binaryPath, binaryName, archivePath)
}

func writeZip(binaryPath, binaryName, archivePath string) (returnErr error) {
	input, err := os.Open(binaryPath)
	if err != nil {
		return err
	}
	defer func(input *os.File) {
		err := input.Close()
		if err != nil {

		}
	}(input)

	output, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); returnErr == nil {
			returnErr = err
		}
	}()

	writer := zip.NewWriter(output)
	header := &zip.FileHeader{Name: binaryName, Method: zip.Deflate}
	header.SetMode(0o755)
	// ZIP 的时间字段从 1980 年开始。固定时间可以避免仅因打包时间不同而产生
	// 不同的归档文件，便于 CI 重复构建和校验。
	header.Modified = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	entry, err := writer.CreateHeader(header)
	if err == nil {
		_, err = io.Copy(entry, input)
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writeTarGzip(binaryPath, binaryName, archivePath string) (returnErr error) {
	input, err := os.Open(binaryPath)
	if err != nil {
		return err
	}
	defer func(input *os.File) {
		err := input.Close()
		if err != nil {

		}
	}(input)
	info, err := input.Stat()
	if err != nil {
		return err
	}

	output, err := os.Create(archivePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := output.Close(); returnErr == nil {
			returnErr = err
		}
	}()

	gzipWriter, err := gzip.NewWriterLevel(output, gzip.BestCompression)
	if err != nil {
		return err
	}
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	tarWriter := tar.NewWriter(gzipWriter)
	header := &tar.Header{
		Name:    binaryName,
		Mode:    0o755,
		Size:    info.Size(),
		ModTime: time.Unix(0, 0).UTC(),
	}
	if err = tarWriter.WriteHeader(header); err == nil {
		_, err = io.Copy(tarWriter, input)
	}
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gzipWriter.Close(); err == nil {
		err = closeErr
	}
	return err
}

func writeChecksums(outputDir string) error {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return fmt.Errorf("读取输出目录失败：%w", err)
	}
	var archiveNames []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, applicationName+"_") {
			continue
		}
		if strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar.gz") {
			archiveNames = append(archiveNames, name)
		}
	}
	sort.Strings(archiveNames)

	var content strings.Builder
	for _, name := range archiveNames {
		file, err := os.Open(filepath.Join(outputDir, name))
		if err != nil {
			return fmt.Errorf("读取 %s 失败：%w", name, err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("计算 %s 校验值失败：%w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭 %s 失败：%w", name, closeErr)
		}
		_, err = fmt.Fprintf(&content, "%s  %s\n", hex.EncodeToString(hash.Sum(nil)), name)
		if err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(outputDir, "checksums.txt"), []byte(content.String()), 0o644); err != nil {
		return fmt.Errorf("写入校验文件失败：%w", err)
	}
	return nil
}

func run(root string, overrides map[string]string, name string, arguments ...string) error {
	command := exec.Command(name, arguments...)
	command.Dir = root
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	command.Env = environmentWithOverrides(overrides)
	if err := command.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("没有找到命令 %q，请先安装并加入 PATH", name)
		}
		return err
	}
	return nil
}

// environmentWithOverrides 会先移除旧值再加入目标值，避免依赖操作系统对重复环境
// 变量的处理顺序。这样从设置过 GOOS 的终端执行，结果仍然是明确且可重复的。
func environmentWithOverrides(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return os.Environ()
	}
	result := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if _, replaced := overrides[name]; !replaced {
			result = append(result, item)
		}
	}
	for name, value := range overrides {
		result = append(result, name+"="+value)
	}
	return result
}

func findProjectRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		content, readErr := os.ReadFile(filepath.Join(directory, "go.mod"))
		if readErr == nil && strings.Contains(string(content), "module iperf3-tool") {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("未找到 iperf3-tool 项目根目录，请在项目目录中运行")
		}
		directory = parent
	}
}

func cleanCommand(root string) error {
	dist := filepath.Join(root, "dist")
	info, err := os.Lstat(dist)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Println("dist 目录不存在，无需清理。")
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查 dist 目录失败：%w", err)
	}
	// 目标固定为当前项目根目录下的 dist，不接受用户传入任意删除路径。
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("dist 是符号链接，为避免误删已拒绝清理，请手动确认")
	}
	if err := os.RemoveAll(dist); err != nil {
		return fmt.Errorf("清理 dist 失败：%w", err)
	}
	fmt.Println("已清理 dist。")
	return nil
}

func fail(err error) {
	_, err = fmt.Fprintln(os.Stderr, "错误：", err)
	if err != nil {
		return
	}
	os.Exit(1)
}
