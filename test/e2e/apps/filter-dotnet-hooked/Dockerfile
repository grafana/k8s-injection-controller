FROM mcr.microsoft.com/dotnet/sdk:9.0.315@sha256:1f2cb07e7ced57c4ea61163d485a2ee1cffd63763c6b17a4447ce734ff236475 AS nop-build
WORKDIR /nop
RUN printf '<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net9.0</TargetFramework></PropertyGroup></Project>' \
        > NopHook.csproj && \
    printf 'public class NopHook { public static void Initialize() { } }' \
        > NopHook.cs && \
    dotnet publish -c Release -o /nop-out

FROM mcr.microsoft.com/dotnet/sdk:9.0.315@sha256:1f2cb07e7ced57c4ea61163d485a2ee1cffd63763c6b17a4447ce734ff236475 AS app-build
WORKDIR /app
COPY . .
RUN dotnet publish -c Release -o /app-out

FROM mcr.microsoft.com/dotnet/aspnet:9.0.17@sha256:5d6784c19991f6cd2b07acbc4f40236e9b90fe4200aebb83c85370fc25a7f248
WORKDIR /app
COPY --from=app-build /app-out .
COPY --from=nop-build /nop-out/NopHook.dll /opt/nop-hook.dll
# This env var is what Beyla's scanner looks for to mark the .NET process incompatible.
ENV DOTNET_STARTUP_HOOKS=/opt/nop-hook.dll
ENTRYPOINT ["dotnet", "App.dll"]
