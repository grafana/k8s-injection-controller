FROM mcr.microsoft.com/dotnet/sdk:9.0.315@sha256:1f2cb07e7ced57c4ea61163d485a2ee1cffd63763c6b17a4447ce734ff236475 AS build
# clang is the platform linker required by Native AOT on Linux.
RUN apt-get update && apt-get install -y --no-install-recommends clang zlib1g-dev && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY . .
# Detect the host architecture and map to the dotnet RID so the image can be
# built on both x86_64 and aarch64 without changing the Dockerfile.
RUN ARCH=$(uname -m) && \
    case "$ARCH" in \
        x86_64)  RID=linux-x64 ;; \
        aarch64) RID=linux-arm64 ;; \
        *) echo "Unsupported arch: $ARCH" && exit 1 ;; \
    esac && \
    dotnet publish -c Release -r "$RID" -o /out

FROM mcr.microsoft.com/dotnet/runtime-deps:9.0.17@sha256:608b519f61bce1ad7496a2544041d6c1538a5c48d056adf55af7fdc35f924283
WORKDIR /app
COPY --from=build /out .
ENTRYPOINT ["./App"]
