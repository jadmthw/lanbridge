// LANBridge AI bridge for the Carpet mod (Minecraft Java Edition 26.3+).
// Lets the LANBridge app on this computer spawn AI-controlled players and
// chat with them. Load it in your world with:   /script load lanbridge
//
// LANBridge talks to this app through files in <world>/scripts/lanbridge.data:
// it drops commands in in/ and reads events from out/. Only Carpet fake
// players can be controlled; real players are never touched.

__config() -> {
    'scope' -> 'global',
    'stay_loaded' -> true
};

global_session = str(unix_time());
global_seq = 0;
global_ticks = 0;
global_out = [];
global_allowed = '^(stop|use|jump|attack|drop|dropStack|swapHands|hotbar|sneak|unsneak|sprint|unsprint|look|turn|move)( [A-Za-z0-9 ._:~^-]{0,80})?$';

__on_start() -> (
    old = list_files('in', 'json');
    if (old, for (old, delete_file(slice(_, 0, length(_) - 5), 'json')));
    lb_hello();
    for (player('all'),
        if (_~'player_type' != 'fake', print(_, '[LANBridge] AI bridge loaded. Add AI players in the LANBridge app, or type !ai help'))
    );
    schedule(1, 'lb_tick')
);

lb_tick() -> (
    global_ticks += 1;
    lb_poll();
    if (global_ticks % 10 == 0, lb_hello());
    lb_flush();
    schedule(2, 'lb_tick')
);

lb_hello() -> (
    everyone = player('all');
    real = filter(everyone, _~'player_type' != 'fake');
    write_file('hello', 'json', {
        'v' -> 1,
        'session' -> global_session,
        'time' -> unix_time(),
        'world' -> system_info('world_name'),
        'players' -> map(real, _~'name'),
        'hosts' -> map(filter(real, lb_is_host(_)), _~'name'),
        'fakes' -> map(filter(everyone, _~'player_type' == 'fake'), _~'name')
    })
);

lb_is_host(p) -> (
    t = p~'player_type';
    t == 'lan_host' || t == 'singleplayer'
);

lb_flush() -> (
    if (global_out,
        global_seq += 1;
        write_file(str('out/e%s_%09d', global_session, global_seq), 'json', {'session' -> global_session, 'events' -> global_out});
        global_out = []
    )
);

lb_poll() -> (
    files = list_files('in', 'json');
    if (files,
        for (sort(files),
            nm = slice(_, 0, length(_) - 5);
            batch = try(read_file(nm, 'json'), 'exception', null);
            delete_file(nm, 'json');
            if (type(batch) == 'map' && type(batch:'cmds') == 'list',
                for (batch:'cmds', lb_exec(_))
            )
        )
    )
);

lb_exec(c) -> (
    opn = c:'op';
    res = try(
        if (opn == 'state', lb_state(c),
            opn == 'act', lb_act(c),
            opn == 'say', lb_say(c),
            opn == 'tell', lb_tell(c),
            opn == 'spawn', lb_spawn(c),
            opn == 'remove', lb_remove(c),
            opn == 'tp', lb_tp(c),
            opn == 'find', lb_find(c),
            opn == 'block', lb_block_at(c),
            opn == 'give', lb_give(c),
            opn == 'select', lb_select(c),
            opn == 'ping', {'ok' -> true},
            {'error' -> 'unknown op ' + opn}
        ),
        'exception', {'error' -> str(_)}
    );
    if (c:'id' != null, global_out += {'type' -> 'result', 'id' -> c:'id', 'data' -> res})
);

lb_fake(nm) -> (
    p = player(nm);
    if (p != null && p~'player_type' == 'fake', p, null)
);

lb_result(r) -> if (r:2, {'error' -> str(r:2)}, {'ok' -> true, 'output' -> r:1});

lb_spawn(c) -> (
    nm = c:'name';
    if (!(nm ~ '^[A-Za-z0-9_]{3,16}$'), return({'error' -> 'bad name'}));
    if (player(nm) != null, return({'error' -> 'someone called ' + nm + ' is already in the world'}));
    near = if (c:'near', player(c:'near'), null);
    if (near == null || near~'player_type' == 'fake', near = first(player('all'), _~'player_type' != 'fake'));
    mode = if (c:'mode' == 'creative', 'creative', 'survival');
    r = if (near != null,
        run(str('execute as %s at @s run player %s spawn in %s', near~'name', nm, mode)),
        run(str('player %s spawn in %s', nm, mode))
    );
    lb_result(r)
);

lb_remove(c) -> (
    p = player(c:'name');
    if (p == null, return({'ok' -> true, 'gone' -> true}));
    if (p~'player_type' != 'fake', return({'error' -> 'not an AI player'}));
    lb_result(run(str('player %s kill', c:'name')))
);

lb_say(c) -> (
    print(player('all'), '<' + c:'name' + '> ' + c:'text');
    {'ok' -> true}
);

lb_tell(c) -> (
    targets = if (c:'to', [player(c:'to')], player('all'));
    for (targets, if (_ != null && _~'player_type' != 'fake', print(_, c:'text')));
    {'ok' -> true}
);

lb_act(c) -> (
    if (lb_fake(c:'name') == null, return({'error' -> 'no such AI player'}));
    act = c:'action';
    if (!(act ~ global_allowed), return({'error' -> 'action not allowed'}));
    lb_result(run('player ' + c:'name' + ' ' + act))
);

lb_tp(c) -> (
    if (lb_fake(c:'name') == null, return({'error' -> 'no such AI player'}));
    if (c:'to',
        if (player(c:'to') == null, return({'error' -> 'player not found'}));
        lb_result(run(str('tp %s %s', c:'name', c:'to'))),
        lb_result(run(str('tp %s %.2f %.2f %.2f', c:'name', c:'x', c:'y', c:'z')))
    )
);

lb_item(h) -> if (h == null, null, {'item' -> h:0, 'count' -> h:1});

lb_inv(p) -> (
    counts = {};
    loop (36,
        it = inventory_get(p, _);
        if (it != null,
            k = it:0;
            v = counts:k;
            put(counts, k, if (v == null, 0, v) + it:1)
        )
    );
    counts
);

lb_players() -> map(filter(player('all'), _~'player_type' != 'fake'),
    pp = _~'pos';
    {'name' -> _~'name', 'x' -> pp:0, 'y' -> pp:1, 'z' -> pp:2, 'dim' -> _~'dimension', 'host' -> lb_is_host(_)}
);

lb_state(c) -> (
    p = player(c:'name');
    if (p == null, return({'online' -> false}));
    ppos = p~'pos';
    rad = if (c:'radius', c:'radius', 20);
    nearby = [];
    for (entity_area('living', ppos, [rad, rad, rad]),
        if (_ != p,
            ep = _~'pos';
            nearby += {'type' -> _~'type', 'name' -> _~'name', 'id' -> _~'id', 'cat' -> _~'category', 'x' -> ep:0, 'y' -> ep:1, 'z' -> ep:2}
        )
    );
    {
        'online' -> true,
        'x' -> ppos:0, 'y' -> ppos:1, 'z' -> ppos:2,
        'yaw' -> p~'yaw', 'pitch' -> p~'pitch',
        'health' -> p~'health', 'food' -> p~'hunger',
        'dim' -> p~'dimension', 'ground' -> p~'on_ground', 'mode' -> p~'gamemode',
        'holds' -> lb_item(p~'holds'),
        'slot' -> p~'selected_slot',
        'inv' -> lb_inv(p),
        'near' -> nearby,
        'items' -> length(entity_area('item', ppos, [5, 3, 5])),
        'players' -> lb_players(),
        'daytime' -> day_time() % 24000
    }
);

lb_find(c) -> (
    p = lb_fake(c:'name');
    if (p == null, return([]));
    want = c:'pattern';
    rad = min(max(c:'radius', 4), 24);
    center = map(p~'pos', floor(_));
    found = [];
    scan(center, [rad, rad, rad], if (str(_) ~ want, found += [_x, _y, _z]));
    found = sort_key(found, (_:0 - center:0) * (_:0 - center:0) + (_:1 - center:1) * (_:1 - center:1) + (_:2 - center:2) * (_:2 - center:2));
    slice(found, 0, min(8, length(found)))
);

lb_block_at(c) -> {'block' -> str(block(c:'x', c:'y', c:'z'))};

lb_give(c) -> (
    p = lb_fake(c:'name');
    if (p == null, return({'error' -> 'no such AI player'}));
    want = c:'item';
    n = c:'count';
    dropped = 0;
    while (dropped < n, 64,
        slot = try(inventory_find(p, want), 'exception', null);
        if (slot == null, break());
        it = inventory_get(p, slot);
        take = min(it:1, n - dropped);
        drop_item(p, slot, take);
        dropped += take
    );
    {'ok' -> dropped > 0, 'dropped' -> dropped}
);

lb_select(c) -> (
    p = lb_fake(c:'name');
    if (p == null, return({'error' -> 'no such AI player'}));
    slot = try(inventory_find(p, c:'item'), 'exception', null);
    if (slot == null, return({'error' -> 'not carrying ' + c:'item'}));
    if (slot > 8,
        it = inventory_get(p, slot);
        cur = p~'selected_slot';
        old = inventory_get(p, cur);
        inventory_set(p, cur, it:1, it:0, it:2);
        if (old == null, inventory_set(p, slot, 0), inventory_set(p, slot, old:1, old:0, old:2)),
        run(str('player %s hotbar %d', c:'name', slot + 1))
    );
    {'ok' -> true}
);

__on_player_message(player, message) -> (
    if (player~'player_type' == 'fake', return());
    global_out += {'type' -> 'chat', 'player' -> player~'name', 'host' -> lb_is_host(player), 'message' -> message};
    if (lower(message) ~ '^!ai( |$)', 'cancel')
);

__on_player_connects(player) -> (
    global_out += {'type' -> 'join', 'player' -> player~'name', 'fake' -> player~'player_type' == 'fake'}
);

__on_player_disconnects(player, reason) -> (
    global_out += {'type' -> 'leave', 'player' -> player~'name', 'fake' -> player~'player_type' == 'fake'}
);

__on_player_dies(player) -> (
    global_out += {'type' -> 'death', 'player' -> player~'name', 'fake' -> player~'player_type' == 'fake'}
);
