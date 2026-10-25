FROM golang:1.26-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /aether ./cmd/aether

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /aether /aether
EXPOSE 2384
ENTRYPOINT ["/aether"]
CMD ["serve"]
