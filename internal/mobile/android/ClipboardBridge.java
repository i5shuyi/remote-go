package dev.oneo.nsgo;

import android.content.*;
import android.database.Cursor;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.net.LocalServerSocket;
import android.net.LocalSocket;
import android.net.Uri;
import android.os.Process;
import android.provider.DocumentsContract;
import android.provider.OpenableColumns;
import android.webkit.MimeTypeMap;
import org.json.JSONArray;
import org.json.JSONObject;
import java.io.*;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.Locale;
import java.util.UUID;

// The Go controller runs in a separate process with the same application UID.
// Only an opted-in session issues clipboard requests over this local socket.
final class ClipboardBridge {
    private static long lastVersion;

    static void start(Context context) {
        Thread thread = new Thread(() -> {
            String name = "remote-go-clip-" + UUID.randomUUID();
            try (LocalServerSocket server = new LocalServerSocket(name)) {
                Files.write(new File(context.getCacheDir(), "remote-go-clip.socket").toPath(),
                        name.getBytes(StandardCharsets.UTF_8));
                while (true) {
                    try (LocalSocket socket = server.accept()) {
                        if (socket.getPeerCredentials().getUid() != Process.myUid()) continue;
                        BufferedReader r = new BufferedReader(new InputStreamReader(socket.getInputStream(), StandardCharsets.UTF_8));
                        JSONObject result;
                        try {
                            result = handle(context, new JSONObject(r.readLine()));
                        } catch (Exception e) {
                            result = new JSONObject().put("error", "Android 无法访问此剪贴板内容");
                        }
                        socket.getOutputStream().write(result.toString().getBytes(StandardCharsets.UTF_8));
                    } catch (Exception ignored) { /* Closing a session may cancel its request. */ }
                }
            } catch (IOException e) {
                android.util.Log.e("RemoteGo", "Clipboard service unavailable", e);
            }
        }, "remote-go-clipboard");
        thread.setDaemon(true);
        thread.start();
    }

    private static JSONObject handle(Context ctx, JSONObject req) throws Exception {
        ClipboardManager cm = ctx.getSystemService(ClipboardManager.class);
        JSONObject out = new JSONObject();
        switch (req.getString("op")) {
            case "version":
                ClipDescription desc = cm.getPrimaryClipDescription();
                if (desc != null) lastVersion = desc.getTimestamp();
                return out.put("version", lastVersion);
            case "text":
                ClipData text = cm.getPrimaryClip();
                if (text != null && text.getItemCount()>0) {
                    CharSequence t = text.getItemAt(0).getText();
                    if (t != null && t.length() <= 32768) out.put("text",t.toString());
                }
                return out;
            case "setText":
                cm.setPrimaryClip(ClipData.newPlainText("",req.getString("text")));
                return out;
            case "get":
                return read(ctx, cm, privateDir(ctx,req.getString("dir")), out);
            case "set":
                JSONArray paths = req.getJSONArray("files");
                boolean image = req.optBoolean("image");
                ClipData clip = null;
                for (int i=0;i<paths.length();i++) {
                    File f=privateDir(ctx,paths.getString(i));
                    if (!f.isFile()) throw new IOException();
                    String name=f.getName();
                    String ext= name.substring(name.lastIndexOf('.')+1).toLowerCase(Locale.ROOT);
                    String mime=image ? "image/png" : MimeTypeMap.getSingleton().getMimeTypeFromExtension(ext);
                    Uri uri=FileProvider.uri(ctx,f,name,mime==null?"application/octet-stream":mime);
                    if (clip==null) clip=ClipData.newUri(ctx.getContentResolver(),name,uri);
                    else clip.addItem(ctx.getContentResolver(),new ClipData.Item(uri));
                }
                if (clip==null) throw new IOException();
                cm.setPrimaryClip(clip);
                return out;
            default: throw new IOException();
        }
    }

    private static File privateDir(Context ctx,String path) throws IOException {
        File f=new File(path).getCanonicalFile();
        String root=new File(ctx.getCacheDir(),"remote-go/clipboard").getCanonicalPath()+File.separator;
        if (!f.getPath().startsWith(root)) throw new IOException();
        return f;
    }

    private static JSONObject read(Context ctx,ClipboardManager cm, File dir, JSONObject out) throws Exception {
        ClipData clip=cm.getPrimaryClip();
        if (clip==null || clip.getItemCount()==0 || clip.getItemAt(0).getUri()==null) return out;
        if (clip.getItemCount()>128) throw new IOException();
        ContentResolver resolver=ctx.getContentResolver();
        JSONArray files=new JSONArray();
        long remaining=1L<<30;
        for (int i=0;i<clip.getItemCount();i++) {
            Uri uri=clip.getItemAt(i).getUri();
            // Only open content granted by another provider. file: URIs can
            // otherwise trick the receiver into reading its own private files.
            if (uri==null || !"content".equals(uri.getScheme()) || FileProvider.authority(ctx).equals(uri.getAuthority())) return out;
            String mime=resolver.getType(uri);
            if (DocumentsContract.Document.MIME_TYPE_DIR.equals(mime)) throw new IOException();
            boolean image=clip.getItemCount()==1 && mime!=null && mime.startsWith("image/")
                    && !DocumentsContract.isDocumentUri(ctx,uri);
            String name="file-"+(i+1);
            try (Cursor cursor=resolver.query(uri,new String[]{OpenableColumns.DISPLAY_NAME},null,null,null)) {
                if (cursor!=null && cursor.moveToFirst() && !cursor.isNull(0)) name=cursor.getString(0);
            }
            if (name.isEmpty() || name.equals(".") || name.equals("..") || name.contains("/") || name.contains("\\")) throw new IOException();
            File sub=new File(dir,String.valueOf(i));
            if (!sub.mkdir()) throw new IOException();
            File f=new File(sub,image?"image.png":name);
            if (image) {
                BitmapFactory.Options size=new BitmapFactory.Options();
                size.inJustDecodeBounds=true;
                try (InputStream in=resolver.openInputStream(uri)) { BitmapFactory.decodeStream(in,null,size); }
                if (size.outWidth<=0 || size.outHeight<=0 || (long)size.outWidth*size.outHeight>(32L<<20)) throw new IOException();
                try (InputStream in=resolver.openInputStream(uri); OutputStream dest=new FileOutputStream(f)) {
                    Bitmap b=BitmapFactory.decodeStream(in);
                    if (b==null) throw new IOException();
                    try { if (!b.compress(Bitmap.CompressFormat.PNG,100,dest)) throw new IOException(); }
                    finally { b.recycle(); }
                }
                if (f.length()>(32L<<20)) throw new IOException();
                return out.put("imagePath",f.getPath());
            }
            try (InputStream in=resolver.openInputStream(uri); OutputStream dest=new FileOutputStream(f)) {
                byte[] buf=new byte[65536];
                int n;
                while ((n=in.read(buf))!=-1) {
                    if (!dir.exists() || (remaining-=n)<0) throw new IOException();
                    dest.write(buf,0,n);
                }
            }
            files.put(f.getPath());
        }
        return out.put("files",files);
    }
}
