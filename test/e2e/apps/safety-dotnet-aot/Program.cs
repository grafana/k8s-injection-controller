var builder = WebApplication.CreateSlimBuilder(args);
var app = builder.Build();

app.MapGet("/", () => "hello from dotnet native aot");

app.Run("http://0.0.0.0:8080");
