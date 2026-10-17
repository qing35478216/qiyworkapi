# 安全政策

## 支持的范围

只对**最新 Release** 提供安全修复。请先把实例升级到最新版本再报告问题。

## 如何报告漏洞

请通过 GitHub 的 **Private vulnerability reporting**（仓库页 → Security → Report a vulnerability）私下提交，**不要开公开 Issue**。

报告请包含：

- 问题描述与复现步骤
- 影响范围（本地部署 / 公网部署、是否配置 `api_key`）
- 修复建议（如有）

## 凭证安全特别说明

本项目在 `auths/` 目录保存 CodeBuddy 账号的**明文 accessToken / refreshToken**，在 `config.json` 保存网关 `api_key`。报告安全问题时：

- **切勿在 Issue、PR 或报告内容中粘贴真实凭证、token 或完整的 auth 文件内容**
- 本地部署请确保 `auths/` 与 `config.json` 权限为 `600`（程序写入时即为 `600`）
- 公网部署必须设置强 `api_key`，并建议置于 TLS 反向代理之后

## 响应预期

- 确认收到：3 个工作日内
- 评估与修复计划：7 个工作日内
- 修复发布后，报告者可在 Release notes 中署名（除非你要求匿名）

## 范围外

以下内容不属于本项目漏洞：

- CodeBuddy / 腾讯系服务端的接口变更或封禁（上游行为，非本项目缺陷）
- 违反[使用声明](README.md#使用声明必读)的用法所引发的问题（批量小号、对外售卖等——本项目不授权此类使用，也不为其提供支持）
