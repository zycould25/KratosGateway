FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kratos-lan ./cmd/kratos-lan

FROM alpine:3.22
RUN addgroup -S app && adduser -S -G app app
USER app
COPY --from=build /out/kratos-lan /usr/local/bin/kratos-lan
ENTRYPOINT ["/usr/local/bin/kratos-lan"]
