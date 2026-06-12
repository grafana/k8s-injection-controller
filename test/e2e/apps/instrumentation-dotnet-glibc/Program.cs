var builder = WebApplication.CreateBuilder(args);
var app = builder.Build();

app.MapGet("/", () => "hello from dotnet");

app.Run("http://0.0.0.0:8080");
