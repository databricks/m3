import json
import numpy as np
f = open("test.txt", "r")

def count(lst):
    cnt = {}
    for x in lst:
        cnt[x] = cnt.get(x, 0) + 1
    return cnt

def count_d(d):
    cnt = {}
    for x in d:
        cnt[len(d[x])] = cnt.get(len(d[x]), 0) + 1
    return cnt

def bucket(t):
    if 0 <= t <= 600:
        return 1
    if 600 < t <= 3600:
        return 2
    if 3600 < t <= 7200:
        return 3
    if 7200 < t <= 21600:
        return 4
    if 21600 < t <= 43200:
        return 5
    if 43200 < t <= 86400:
        return 6
    if 86400 < t <= 259200:
        return 7
    return 8


cache_miss = 0
cache_hit = 0

cached_bytes = 0
read_bytes = 0

cached_samples = 0
read_samples = 0

prev_hit = False

response_length = 0
response_cnt = 0
response_time = 0

start = 0
ranges = []
windows = {}
times = []
for line in f:
    # skip if not full log statement
    if line[0] != '{':
        continue
    data = json.loads(line)
    # Set time range
    # if start == 0:
    #     start = data['ts']
    # elif data['ts'] - start >= 300:
    #     break
    if "cache miss" in line:
        cache_miss += 1
        read_bytes += data['bytes']
        # read_samples += data.get('num_samples', 0)
        prev_hit = False
    elif "cache hit" in line:
        cache_hit += 1
        cached_bytes += data['bytes']
        # cached_samples += data.get('num_samples', 0)
        prev_hit = True
    
    # This logic depends on the fact that the response log comes right after cache hit/miss
    # This is not necessary if the logs are from an image with the latest version with updated logs
    # With latest version you can just accumulate above by uncommenting samples lines
    # if "fetch response" in line:
    #     # if prev_hit:
    #     #     cached_samples += np.sum(data.get('sample_cnts', [0]))
    #     # else:
    #     #     read_samples += np.sum(data.get('sample_cnts', [0]))
    #     # prev_hit = False
    #     response_length += data['prom_result_len']
    #     response_time += data['elapsed']
    #     response_cnt += 1

    # if "fetch query" in line:
    #     ranges.append(data['range'])
    #     if data['key'] not in windows:
    #         windows[data['key']] = set()
    #     windows[data['key']].add(bucket(data['range']))

    if "fetch response" in line:
        times.append(data['elapsed'])

# print(np.average(ranges))
# 0.007333032179541087 [0.00554004 0.13021972]
# 0.011810705072786832 [0.00704725 0.18004101]
print(np.average(times), np.quantile(times, [0.9, 0.99]))


# print("Hit Rate:", cache_hit / (cache_miss + cache_hit))
# print("Sample Ratio:", (cached_samples) / (cached_samples + read_samples))
# print("Byte Ratio:", cached_bytes / (cached_bytes + read_bytes))