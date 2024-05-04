FROM golang:1.21.6-alpine as marmounter

RUN apk --no-cache add fuse-dev gcc musl-dev

WORKDIR /mayakashi
COPY go.mod go.sum .
RUN go mod download

COPY proto ./proto
COPY marmounter ./marmounter
RUN go build -o marmounter.exe ./marmounter

FROM rust:1.72.0-alpine as mayakashi

RUN apk --no-cache add musl-dev protoc protobuf-dev

WORKDIR /mayakashi
COPY Cargo.toml Cargo.lock .
RUN mkdir src && touch src/main.rs && cargo fetch

COPY build.rs .
COPY proto ./proto
COPY src/ ./src/
RUN cargo build --release

FROM python:3.12-alpine

RUN apk --no-cache add fuse

WORKDIR /mayakashi
COPY --from=marmounter /mayakashi/marmounter.exe .
COPY --from=mayakashi /mayakashi/target/release/mayakashi ./mayakashi.exe
COPY tests ./tests

CMD ["python3", "tests/e2e.py"]