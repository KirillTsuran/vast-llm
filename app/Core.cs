// Vast.ai logic: config/state next to the exe, offers, renting (2-host race), SSH tunnel, auto-down, watchdog heartbeat.
using System.Globalization;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Json.Serialization;
using System.Text.RegularExpressions;
using Renci.SshNet;

namespace VastLLM;

public class Config
{
    [JsonPropertyName("vast_api_key")] public string VastKey { get; set; } = "";
    [JsonPropertyName("gpu")] public string Gpu { get; set; } = "RTX 3090";
    [JsonPropertyName("max_price_usd_per_hour")] public double MaxDph { get; set; } = 0.40;
    [JsonPropertyName("datacenter_only")] public bool Datacenter { get; set; } = false;
    [JsonPropertyName("race_two_hosts")] public bool Race { get; set; } = true;
    [JsonPropertyName("local_port")] public int LocalPort { get; set; } = 8080;
    [JsonPropertyName("image")] public string Image { get; set; } = "ghcr.io/kirilltsuran/vast-llm:latest";
    [JsonPropertyName("model")] public string Model { get; set; } = "qwen3.8-27b-uncensored";
    [JsonPropertyName("usd_rub")] public double UsdRub { get; set; } = 83.56;
}

public class State
{
    [JsonPropertyName("instance_id")] public long Id { get; set; }
    [JsonPropertyName("gpu")] public string Gpu { get; set; } = "";
    [JsonPropertyName("geo")] public string Geo { get; set; } = "";
    [JsonPropertyName("usd_per_hour")] public double Dph { get; set; }
    [JsonPropertyName("created")] public DateTime Created { get; set; }
    [JsonPropertyName("host_key")] public string HostKey { get; set; } = "";
    [JsonPropertyName("bad_machines")] public List<long> BadMachines { get; set; } = new();
}

public record Offer(long Id, long Machine, double Dph, double InetCost, double Inet, string Geo, string Gpu);

public class Core
{
    public const string Label = "vast-llm";
    static readonly JsonSerializerOptions JsonOpt = new() { WriteIndented = true };
    public readonly string Dir = AppContext.BaseDirectory;
    string P(string f) => Path.Combine(Dir, f);

    public Config Cfg = new();
    public State St = new();
    public string Phase = "off", Message = "";
    public event Action Changed;
    public event Action<string> Logged;

    SshClient client;
    ForwardedPortLocal fwd;
    PrivateKeyFile key;
    string pub;
    readonly SemaphoreSlim busy = new(1, 1);
    CancellationTokenSource upCts;
    DateTime lastUse = DateTime.UtcNow;
    readonly HttpClient http = new() { Timeout = TimeSpan.FromSeconds(40) };
    readonly object stateLock = new();

    public bool Tunnel => client?.IsConnected == true && fwd?.IsStarted == true;
    public int IdleMin => (int)(DateTime.UtcNow - lastUse).TotalMinutes;

    // ---------- files ----------
    public string LoadConfig()
    {
        var path = P("config.json");
        if (!File.Exists(path)) { SaveConfig(); return "создан config.json рядом с программой: впишите vast_api_key"; }
        try { Cfg = JsonSerializer.Deserialize<Config>(File.ReadAllText(path)) ?? new Config(); SaveConfig(); }
        catch (Exception e) { return "config.json не читается: " + e.Message; }
        return Cfg.VastKey.Trim() == "" ? "впишите vast_api_key в config.json и нажмите Up" : null;
    }
    public void SaveConfig() => File.WriteAllText(P("config.json"), JsonSerializer.Serialize(Cfg, JsonOpt));
    void SaveState() { lock (stateLock) File.WriteAllText(P("state.json"), JsonSerializer.Serialize(St, JsonOpt)); }
    public void LoadState()
    {
        try { if (File.Exists(P("state.json"))) St = JsonSerializer.Deserialize<State>(File.ReadAllText(P("state.json"))) ?? new State(); } catch { St = new State(); }
    }

    public void Log(string s)
    {
        var line = DateTime.Now.ToString("HH:mm:ss ") + s;
        try { File.AppendAllText(P("vast-llm.log"), DateTime.Now.ToString("yyyy-MM-dd ") + line + Environment.NewLine); } catch { }
        Logged?.Invoke(line);
    }
    void Set(string phase, string msg)
    {
        bool ch = Phase != phase || Message != msg;
        Phase = phase; Message = msg;
        if (ch) { Log($"[{phase}] {msg}"); Changed?.Invoke(); }
    }

    // RSA key for SSH (kept next to the exe); public key goes to the container via env
    public void LoadKey()
    {
        var kp = P("ssh_key.pem");
        if (!File.Exists(kp)) { using var rsa = RSA.Create(3072); File.WriteAllText(kp, rsa.ExportRSAPrivateKeyPem()); }
        using (var rsa = RSA.Create())
        {
            rsa.ImportFromPem(File.ReadAllText(kp));
            var p = rsa.ExportParameters(false);
            var ms = new MemoryStream();
            void Str(byte[] b) { var l = BitConverter.GetBytes(b.Length); Array.Reverse(l); ms.Write(l); ms.Write(b); }
            byte[] Mp(byte[] b) => (b[0] & 0x80) != 0 ? new byte[] { 0 }.Concat(b).ToArray() : b;
            Str(Encoding.ASCII.GetBytes("ssh-rsa")); Str(Mp(p.Exponent)); Str(Mp(p.Modulus));
            pub = "ssh-rsa " + Convert.ToBase64String(ms.ToArray()) + " " + Label;
        }
        key = new PrivateKeyFile(kp);
    }

    // ---------- CBR rate ----------
    public async Task RateAsync()
    {
        try
        {
            var b = await http.GetByteArrayAsync("https://www.cbr.ru/scripts/XML_daily.asp");
            var m = Regex.Match(Encoding.Latin1.GetString(b), @"<CharCode>USD</CharCode>.*?<Value>([0-9,]+)</Value>", RegexOptions.Singleline);
            if (m.Success && double.TryParse(m.Groups[1].Value.Replace(',', '.'), NumberStyles.Float, CultureInfo.InvariantCulture, out var v) && v > 0)
            { Cfg.UsdRub = v; SaveConfig(); Log($"курс ЦБ: {v:F2} ₽/$"); Changed?.Invoke(); }
        }
        catch { Log($"курс ЦБ недоступен, использую {Cfg.UsdRub:F2}"); }
    }

    // ---------- Vast API ----------
    async Task<JsonNode> Api(HttpMethod m, string path, object body = null)
    {
        var url = (path.StartsWith("/v1/") ? "https://console.vast.ai/api" : "https://console.vast.ai/api/v0") + path;
        Exception last = null;
        for (int i = 0; i < 4; i++)
        {
            try
            {
                var req = new HttpRequestMessage(m, url);
                req.Headers.Authorization = new AuthenticationHeaderValue("Bearer", Cfg.VastKey.Trim());
                if (body != null) req.Content = new StringContent(JsonSerializer.Serialize(body), Encoding.UTF8, "application/json");
                var r = await http.SendAsync(req);
                var txt = await r.Content.ReadAsStringAsync();
                if (r.IsSuccessStatusCode) return string.IsNullOrWhiteSpace(txt) ? null : JsonNode.Parse(txt);
                last = new Exception($"HTTP {(int)r.StatusCode}: {txt[..Math.Min(300, txt.Length)]}");
                if ((int)r.StatusCode < 500 && (int)r.StatusCode != 429) throw last;
            }
            catch (Exception e) when (e != last) { last = e; }
            await Task.Delay(3000 * (i + 1));
        }
        throw last;
    }

    async Task<List<JsonNode>> Instances() => (await Api(HttpMethod.Get, "/v1/instances"))?["instances"]?.AsArray().ToList() ?? new();
    async Task<JsonNode> Instance(long id) => (await Instances()).FirstOrDefault(i => (long)i["id"] == id);

    async Task<List<Offer>> Offers()
    {
        var q = new Dictionary<string, object>
        {
            ["type"] = "on-demand", ["limit"] = 200, ["num_gpus"] = new { eq = 1 }, ["gpu_name"] = new { eq = Cfg.Gpu },
            ["rentable"] = new { eq = true }, ["rented"] = new { eq = false }, ["verified"] = new { eq = true },
            ["cuda_max_good"] = new { gte = 12.8 }, ["disk_space"] = new { gte = 60 }, ["cpu_ram"] = new { gte = 32000 },
            ["inet_down"] = new { gte = 500 }, ["direct_port_count"] = new { gte = 1 }, ["reliability"] = new { gte = 0.98 },
            ["order"] = new[] { new[] { "dph_total", "asc" } }, ["allocated_storage"] = 60,
        };
        if (Cfg.Datacenter) q["datacenter"] = new { eq = true };
        var r = await Api(HttpMethod.Post, "/bundles/", q);
        var list = new List<Offer>();
        foreach (var o in r?["offers"]?.AsArray() ?? new JsonArray())
        {
            var off = new Offer((long)o["id"], (long)o["machine_id"], (double)o["dph_total"], (double?)o["inet_down_cost"] ?? 0,
                                (double?)o["inet_down"] ?? 0, (string)o["geolocation"] ?? "", (string)o["gpu_name"] ?? "");
            if (off.Dph <= Cfg.MaxDph && off.InetCost <= 0.01 && !St.BadMachines.Contains(off.Machine) && !off.Geo.Contains("CN")) list.Add(off);
        }
        list = list.OrderBy(o => o.Dph + 20 * o.InetCost).ToList();
        if (list.Count > 1) // among offers within $0.05/h of the cheapest, fastest network first
        {
            var lim = list[0].Dph + 0.05;
            var near = list.Where(o => o.Dph <= lim).OrderByDescending(o => o.Inet).ToList();
            list = near.Concat(list.Where(o => o.Dph > lim)).ToList();
        }
        return list;
    }

    async Task Destroy(long id)
    {
        try { await Api(HttpMethod.Delete, $"/instances/{id}/"); } catch (Exception e) { Log($"delete {id}: {e.Message}"); }
        for (int i = 0; i < 40; i++)
        {
            try { if (await Instance(id) == null) { Log($"машина {id} удалена (подтверждено API)"); return; } } catch { }
            await Task.Delay(5000);
        }
        throw new Exception($"удаление {id} не подтверждено, проверьте console.vast.ai");
    }

    async Task<long> Rent(Offer o)
    {
        var pk = Convert.ToBase64String(Encoding.UTF8.GetBytes(pub));
        var body = new Dictionary<string, object>
        {
            ["image"] = Cfg.Image, ["label"] = Label, ["disk"] = 60, ["runtype"] = "args", ["args"] = new[] { "bash", "/opt/llm/entry.sh" },
            ["target_state"] = "running", ["cancel_unavail"] = true, ["env"] = $"-e PUBKEY_B64={pk} -e WATCHDOG_MIN=525600 -p 22:22",
        };
        var r = await Api(HttpMethod.Put, $"/asks/{o.Id}/", body);
        var id = (long?)r?["new_contract"] ?? 0;
        if (r?["success"]?.GetValue<bool>() != true || id == 0) throw new Exception("offer not accepted");
        return id;
    }

    // waits for the container and opens SSH to its own sshd (port 22 mapped to a random external port)
    async Task<(SshClient, string)> Dial(long id, string pinned, TimeSpan boot, CancellationToken ct)
    {
        var deadline = DateTime.UtcNow + boot; DateTime? running = null; Exception last = new("машина не запустилась вовремя");
        while (DateTime.UtcNow < deadline)
        {
            ct.ThrowIfCancellationRequested();
            JsonNode inst = null;
            try { inst = await Instance(id); if (inst == null) throw new Exception("машина исчезла"); }
            catch (Exception e) when (e.Message == "машина исчезла") { throw; }
            catch { }
            if (inst != null && (string)inst["actual_status"] == "running" && !string.IsNullOrEmpty((string)inst["public_ipaddr"]))
            {
                running ??= DateTime.UtcNow;
                var port = int.TryParse((string)inst["ports"]?["22/tcp"]?[0]?["HostPort"], out var pp) ? pp : 0;
                if (port > 0)
                {
                    var hk = pinned;
                    var ci = new ConnectionInfo((string)inst["public_ipaddr"], port, "root", new PrivateKeyAuthenticationMethod("root", key)) { Timeout = TimeSpan.FromSeconds(15) };
                    var c = new SshClient(ci) { KeepAliveInterval = TimeSpan.FromSeconds(20) };
                    c.HostKeyReceived += (_, e) =>
                    {
                        if (string.IsNullOrEmpty(hk)) { hk = e.FingerPrintSHA256; e.CanTrust = true; }
                        else e.CanTrust = hk == e.FingerPrintSHA256;
                    };
                    try { await Task.Run(() => c.Connect(), ct); return (c, hk); }
                    catch (Exception e) { last = e; c.Dispose(); }
                }
                if (DateTime.UtcNow - running > TimeSpan.FromMinutes(3)) throw new Exception("SSH недоступен 3 мин после старта: " + last.Message);
            }
            await Task.Delay(6000, ct);
        }
        throw last;
    }

    void Attach(SshClient c)
    {
        CloseClient();
        client = c;
        fwd = new ForwardedPortLocal("127.0.0.1", (uint)Cfg.LocalPort, "127.0.0.1", 8080);
        fwd.RequestReceived += (_, _) => lastUse = DateTime.UtcNow;
        client.AddForwardedPort(fwd);
        fwd.Start();
        Exec("touch /opt/llm/heartbeat");
    }

    void CloseClient()
    {
        try { fwd?.Stop(); } catch { }
        try { client?.Dispose(); } catch { }
        fwd = null; client = null;
    }

    string Exec(string cmd)
    {
        try { return client?.IsConnected == true ? client.RunCommand(cmd).Result : null; } catch { return null; }
    }

    // ---------- Up / Down ----------
    public async Task Up()
    {
        var err = LoadConfig();
        if (err != null && Cfg.VastKey.Trim() == "") { Set("error", err); return; }
        if (!await busy.WaitAsync(0)) return;
        upCts = new CancellationTokenSource(); var ct = upCts.Token;
        var start = DateTime.UtcNow;
        try
        {
            if (St.Id != 0 && !Tunnel)
            {
                Set("booting", $"переподключаюсь к машине {St.Id}");
                try { var (c, hk) = await Dial(St.Id, St.HostKey, TimeSpan.FromMinutes(3), ct); St.HostKey = hk; SaveState(); Attach(c); }
                catch (OperationCanceledException) { throw; }
                catch (Exception e) { Set("error", $"машина {St.Id} не отвечает по SSH ({e.Message}). Она не удалена: Up — повторить, Down — удалить"); return; }
            }
            for (int round = 1; St.Id == 0 && round <= 3; round++)
            {
                var offs = await Offers();
                if (offs.Count == 0) { Set("error", $"нет {Cfg.Gpu} дешевле ${Cfg.MaxDph}/ч — поднимите max_price_usd_per_hour"); return; }
                var picks = offs.Take(Cfg.Race ? 2 : 1).ToList();
                Set("renting", picks.Count > 1 ? $"арендую 2 машины {Cfg.Gpu}, оставлю первую поднявшуюся" : $"арендую {Cfg.Gpu}");
                using var race = CancellationTokenSource.CreateLinkedTokenSource(ct);
                var tasks = picks.Select(o => Task.Run(async () =>
                {
                    long id = 0;
                    try
                    {
                        id = await Rent(o);
                        Log($"арендована {id}: {o.Gpu} {o.Geo} {o.Dph * Cfg.UsdRub:F1} ₽/ч, сеть {(int)o.Inet} Мбит/с");
                        var (c, hk) = await Dial(id, "", TimeSpan.FromMinutes(10), race.Token);
                        return (o, id, c, hk, (Exception)null);
                    }
                    catch (Exception e) { return (o, id, (SshClient)null, "", e); }
                })).ToList();
                Set("booting", "машина качает образ и запускается (~5 мин)");
                bool won = false;
                while (tasks.Count > 0)
                {
                    var t = await Task.WhenAny(tasks); tasks.Remove(t);
                    var (o, id, c, hk, e) = t.Result;
                    if (e != null || won)
                    {
                        if (e != null && !won && e is not OperationCanceledException) { Log($"хост {o.Machine} не подошёл: {e.Message}"); St.BadMachines.Add(o.Machine); }
                        c?.Dispose();
                        if (id != 0) _ = Destroy(id);
                        continue;
                    }
                    won = true; race.Cancel();
                    St = new State { Id = id, Gpu = o.Gpu, Geo = o.Geo, Dph = o.Dph, Created = DateTime.UtcNow, HostKey = hk, BadMachines = St.BadMachines };
                    SaveState(); Attach(c);
                    Log($"выбрана машина {id} ({o.Geo}), SSH через {(DateTime.UtcNow - start):mm\\:ss}");
                }
                ct.ThrowIfCancellationRequested();
            }
            if (St.Id == 0) { Set("error", "не удалось поднять машину за 3 попытки"); return; }
            var deadline = DateTime.UtcNow.AddMinutes(40);
            while (DateTime.UtcNow < deadline)
            {
                ct.ThrowIfCancellationRequested();
                var f = (Exec("cat /opt/llm/state 2>/dev/null; du -sh /opt/llm/models 2>/dev/null | cut -f1") ?? "").Split((char[])null, StringSplitOptions.RemoveEmptyEntries);
                switch (f.FirstOrDefault())
                {
                    case "ready":
                        lastUse = DateTime.UtcNow;
                        Set("ready", $"готово за {(DateTime.UtcNow - start):mm\\:ss}");
                        return;
                    case "download-failed": Set("error", "скачивание модели не удалось: Down, затем Up"); return;
                    case "downloading": Set("downloading", "качаю модель (~18 ГБ): " + (f.Length > 1 ? f[1] : "")); break;
                    case null: Set("booting", "контейнер запускается"); break;
                    default: Set("loading", "загружаю модель в GPU"); break;
                }
                await Task.Delay(5000, ct);
            }
            Set("error", "модель не загрузилась за 40 минут");
        }
        catch (OperationCanceledException) { }
        catch (Exception e) { Set("error", e.Message); }
        finally { busy.Release(); }
    }

    public async Task Down(string reason)
    {
        upCts?.Cancel();
        await busy.WaitAsync();
        try
        {
            if (St.Id == 0) { Set("off", "машин нет"); return; }
            Set("stopping", $"удаляю машину ({reason})");
            CloseClient();
            await Destroy(St.Id);
            St = new State { BadMachines = St.BadMachines }; SaveState();
            Set("off", "машина удалена, оплата остановлена");
        }
        catch (Exception e) { Set("error", e.Message); }
        finally { busy.Release(); }
    }

    // ---------- every 30 s: keep the tunnel alive (machines are deleted only by the Down button) ----------
    public async Task Tick()
    {
        if (St.Id == 0 || busy.CurrentCount == 0) return;
        if (!Tunnel)
        {
            Log("связь потеряна, переподключаюсь");
            try
            {
                if (await Instance(St.Id) == null) { CloseClient(); St = new State { BadMachines = St.BadMachines }; SaveState(); Set("error", "машина пропала у Vast — нажмите Up"); return; }
                var (c, _) = await Dial(St.Id, St.HostKey, TimeSpan.FromMinutes(3), CancellationToken.None);
                Attach(c); Log("связь восстановлена");
            }
            catch (Exception e) { Log("переподключение не удалось: " + e.Message); return; }
        }
        Exec("touch /opt/llm/heartbeat");
    }

    public async Task Startup()
    {
        LoadKey(); LoadState();
        var err = LoadConfig();
        if (err != null) Set("error", err);
        _ = RateAsync();
        if (Cfg.VastKey.Trim() == "") return;
        if (St.Id == 0)
        {
            try
            {
                var mine = (await Instances()).FirstOrDefault(i => (string)i["label"] == Label);
                if (mine != null) { St = new State { Id = (long)mine["id"], Gpu = (string)mine["gpu_name"] ?? "", Geo = (string)mine["geolocation"] ?? "", Dph = (double?)mine["dph_total"] ?? 0, Created = DateTime.UtcNow, BadMachines = St.BadMachines }; SaveState(); }
            }
            catch { }
        }
        if (St.Id != 0) { Log($"найдена работающая машина {St.Id} — подключаюсь"); await Up(); }
    }
}
