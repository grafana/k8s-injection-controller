FROM mcr.microsoft.com/dotnet/sdk:9.0 AS nop-build
WORKDIR /nop
RUN printf '<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net9.0</TargetFramework></PropertyGroup></Project>' \
        > NopHook.csproj && \
    printf 'public class NopHook { public static void Initialize() { } }' \
        > NopHook.cs && \
    dotnet publish -c Release -o /nop-out

FROM mcr.microsoft.com/dotnet/sdk:9.0 AS app-build
WORKDIR /app
COPY . .
RUN dotnet publish -c Release -o /app-out

FROM mcr.microsoft.com/dotnet/aspnet:9.0
WORKDIR /app
COPY --from=app-build /app-out .
COPY --from=nop-build /nop-out/NopHook.dll /opt/nop-hook.dll
# This env var is what Beyla's scanner looks for to mark the .NET process incompatible.
ENV DOTNET_STARTUP_HOOKS=/opt/nop-hook.dll
ENTRYPOINT ["dotnet", "App.dll"]
