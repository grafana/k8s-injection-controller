FROM mcr.microsoft.com/dotnet/sdk:9.0.315@sha256:1f2cb07e7ced57c4ea61163d485a2ee1cffd63763c6b17a4447ce734ff236475 AS build
WORKDIR /app
COPY . .
RUN dotnet publish -c Release -o /out

FROM mcr.microsoft.com/dotnet/aspnet:9.0.17@sha256:5d6784c19991f6cd2b07acbc4f40236e9b90fe4200aebb83c85370fc25a7f248
WORKDIR /app
COPY --from=build /out .
ENTRYPOINT ["dotnet", "App.dll"]
