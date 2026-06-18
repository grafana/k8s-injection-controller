FROM mcr.microsoft.com/dotnet/sdk:9.0.315 AS build
WORKDIR /app
COPY . .
RUN dotnet publish -c Release -o /out

FROM mcr.microsoft.com/dotnet/aspnet:9.0.17
WORKDIR /app
COPY --from=build /out .
ENTRYPOINT ["dotnet", "App.dll"]
