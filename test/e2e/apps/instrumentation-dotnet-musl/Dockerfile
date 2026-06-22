FROM mcr.microsoft.com/dotnet/sdk:9.0.315-alpine3.23@sha256:2412b9f7db4aa7ceac5a8c9d41385616cbd7bb8d18c298334cafad22f7647e04 AS build
WORKDIR /app
COPY . .
RUN dotnet publish -c Release -o /out

FROM mcr.microsoft.com/dotnet/aspnet:9.0.17-alpine3.23@sha256:99a749b0dadd9e11d30d3804d94c8f1edb06db00148df52814219d5ff838f551
WORKDIR /app
COPY --from=build /out .
ENTRYPOINT ["dotnet", "App.dll"]
