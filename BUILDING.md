# 构建与按需发布

项目已经提供跨平台构建入口。默认只构建当前电脑对应的平台和架构；只有明确
指定 `TARGETS` 时，才会构建其他目标，不会默认把全部平台都打一遍。

## 准备环境

发布机器需要：

- Go 1.26 或更高版本。
- Node.js 和 npm，仅在构建内嵌网页资源时使用。
- Task，可选。没有安装 Task 也可以直接使用项目内的 Go 构建程序。

Task 可以使用 Go 安装：

```bash
go install github.com/go-task/task/v3/cmd/task@v3.53.1
```

安装后请确保 Go 的二进制目录已经加入系统的 `PATH`，然后用下面的命令确认：

```bash
task --version
```

## 查看可选目标

```bash
task targets
```

当前提供以下经过项目约束筛选的桌面和服务器目标：

```text
darwin/amd64    macOS Intel
darwin/arm64    macOS Apple 芯片
linux/amd64     Linux x86-64
linux/arm64     Linux ARM64
windows/amd64   Windows x86-64
windows/arm64   Windows ARM64
```

## 常用打包方式

不提供目标时，只构建当前电脑的包：

```bash
task package VERSION=v1.0.0
```

只构建 Windows x86-64：

```bash
task package TARGETS=windows/amd64 VERSION=v1.0.0
```

一次选择多个目标，目标之间使用英文逗号分隔：

```bash
task package TARGETS=windows/amd64,linux/arm64 VERSION=v1.0.0
```

例如，在 Apple 芯片 Mac 上同时构建自己使用的 macOS 包和常见 Linux 服务器包：

```bash
task package TARGETS=darwin/arm64,linux/amd64 VERSION=v1.0.0
```

生成结果位于 `dist`：

```text
dist/
├── iperf3-tool_v1.0.0_darwin_arm64.tar.gz
├── iperf3-tool_v1.0.0_linux_amd64.tar.gz
└── checksums.txt
```

Windows 使用 ZIP，Linux 和 macOS 使用 tar.gz。每个压缩包里只有一个可以运行的
`iperf3-tool` 二进制文件；网页、样式和脚本已经嵌入其中。运行机器仍需另外准备
`iperf3`，这与程序原有约定一致。

## 没有安装 Task 时

Task 只是短命令入口，以下命令的功能完全相同：

```bash
go run ./cmd/build list
go run ./cmd/build package -targets windows/amd64,linux/arm64 -version v1.0.0
go run ./cmd/build clean
```

## 构建过程

一次正式打包会自动完成：

1. 使用 `npm ci` 按 `webui/package-lock.json` 安装前端依赖。
2. 构建前端并更新 `internal/web/assets` 中的内嵌资源。
3. 对选中的目标设置 `CGO_ENABLED=0`、`GOOS` 和 `GOARCH` 并编译。
4. 生成 ZIP 或 tar.gz，并更新 `dist/checksums.txt`。

如果前端已经构建完毕，可以使用底层参数跳过前端构建：

```bash
go run ./cmd/build package -targets linux/amd64 -version v1.0.0 -skip-frontend
```

日常手工发布不建议跳过前端构建。

## 清理发布结果

```bash
task clean
```

该命令只删除当前项目根目录中的 `dist`，并且遇到符号链接时会拒绝删除。

## 运行发布包

Linux 或 macOS 解压后：

```bash
./iperf3-tool
```

Windows 解压后，在 PowerShell 中运行：

```powershell
.\iperf3-tool.exe
```

本机打开 <http://localhost:8088/>；同一局域网的其他设备打开
`http://运行机器的局域网IP:8088/`。如果访问失败，请确认两台设备位于同一网络，
并允许系统防火墙放行该程序的 `8088` 端口。macOS 和 Windows 的公开分发包如果
没有进行开发者签名，系统可能显示安全提醒；代码签名和公证属于发行身份配置，
不影响这里的跨平台编译结果。
