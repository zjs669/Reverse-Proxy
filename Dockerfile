# 去掉 syntax 指令:避免额外从 docker.io 拉 docker/dockerfile 前端镜像(国内常超时)
# 改用 BuildKit 内置 frontend,减少一个外部依赖。
FROM golang:1.23-alpine AS build
# 国内 Go 模块代理(默认 proxy.golang.org 被墙)；GOSUMDB 走 sum.golang.org 同样被墙，关闭校验
ENV GOPROXY=https://goproxy.cn,direct
ENV GOSUMDB=off
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/reverse-proxy ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/reverse-proxy /app/reverse-proxy
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
# 全部运维脚本随镜像（容器内有 python3）：trae/qoder/doubao 凭证生成、账号探活等，
# 可直接 `docker exec -it reverse-proxy python3 /app/scripts/doubao_session.py --help`。
COPY scripts/ /app/scripts/
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/*.sh /app/reverse-proxy /app/login /app/signin_bin /app/credit
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/reverse-proxy", "-config", "/app/config.json"]