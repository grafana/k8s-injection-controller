import java.io.*;
import java.net.*;

// Minimal HTTP server compiled to a GraalVM native image. It uses only raw
// sockets — no JDK reflection, no sun.net.httpserver — so native-image needs
// no extra configuration. The app exists solely to verify that LD_PRELOAD of
// libotelinject.so does not crash a native binary.
public class NativeHttpServer {
    public static void main(String[] args) throws Exception {
        ServerSocket server = new ServerSocket(8080);
        System.out.println("Listening on port 8080");
        while (true) {
            try (Socket client = server.accept();
                 OutputStream out = client.getOutputStream()) {
                // Drain request headers (read until the blank CRLF line).
                InputStream in = client.getInputStream();
                int b, prev = -1, prev2 = -1, prev3 = -1;
                while ((b = in.read()) != -1) {
                    if (prev3 == '\r' && prev2 == '\n' && prev == '\r' && b == '\n') break;
                    prev3 = prev2; prev2 = prev; prev = b;
                }
                byte[] body = "hello from graalvm native\n".getBytes();
                out.write(("HTTP/1.1 200 OK\r\nContent-Length: " + body.length + "\r\nConnection: close\r\n\r\n").getBytes());
                out.write(body);
                out.flush();
            } catch (IOException ignored) {}
        }
    }
}
