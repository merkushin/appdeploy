FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go message.txt ./
RUN CGO_ENABLED=0 go build -o /server .

FROM scratch
COPY --from=build /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
