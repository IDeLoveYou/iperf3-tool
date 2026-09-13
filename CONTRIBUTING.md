# 贡献指南

感谢你关注 iperf3-tool。欢迎提交问题反馈、功能建议和 Pull Request。

## 开始之前

请先搜索已有 Issue，避免重复提交。新问题尽量提供：

- 操作系统和处理器架构；
- Go、Node.js/npm 和 iperf3 版本；
- 检测协议、方向、服务端端口和关键参数；
- 页面显示的完整错误文本；
- 脱敏后的启动日志或复现步骤。

不要提交服务端密码、访问令牌、内网拓扑或其他敏感信息。

## 本地开发

克隆仓库后，在项目根目录执行：

~~~bash
go version
go test ./...
go vet ./...
go build ./...
~~~

如果修改了前端源码：

~~~bash
npm --prefix webui ci
npm --prefix webui run build
~~~

如果修改了构建逻辑或发布相关内容，至少验证：

~~~bash
go run ./cmd/build list
go run ./cmd/build package -targets current -version dev
~~~

发布产物位于 dist 目录。该目录被 Git 忽略，不应提交到仓库。

## 代码约定

- 使用 Go 标准格式化工具，保持包职责清晰。
- 公共类型、函数和关键并发逻辑应有清晰注释。
- 错误信息应说明用户可以采取的下一步操作。
- 不要把前端生成文件当作唯一修改位置；前端逻辑应先修改 webui/src。
- 不要引入不必要的运行时依赖。
- 不要在代码、日志和文档中写入真实凭据或未脱敏的内部信息。
- 修改网络指标计算时，请同时检查实时数据、区间汇总和累计汇总。

## Pull Request

Pull Request 描述建议包含：

1. 修改目的和用户可见行为；
2. 影响的模块；
3. 验证过的命令；
4. 是否需要更新 README、BUILDING 或 API 说明；
5. 是否存在兼容性或部署注意事项。

一个 Pull Request 尽量只解决一个主题。提交前请确认没有意外的构建产物、编辑器文件或本地配置文件。

