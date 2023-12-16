document.addEventListener('DOMContentLoaded', function () {
    const recordButton = document.getElementById('recordButton');
    const progressRing = document.querySelector('.progress-ring__circle');
    const radius = progressRing.r.baseVal.value;
    const circumference = 2 * Math.PI * radius;
    const maxRecordTime = 10000; // 10 seconds
    let recording = false;
    let startTime;
    let mediaRecorder;
    let audioChunks = [];

    progressRing.style.strokeDasharray = `${circumference} ${circumference}`;
    progressRing.style.strokeDashoffset = circumference;

    function setProgress(percent) {
        const offset = circumference - (percent / 100) * circumference;
        progressRing.style.strokeDashoffset = offset;
    }

    async function startRecording() {
        if (recording) return;
        recording = true;
        audioChunks = [];
        startTime = Date.now();
        requestAnimationFrame(updateProgress);

        try {
            const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
            mediaRecorder = new MediaRecorder(stream);
            mediaRecorder.start();

            mediaRecorder.ondataavailable = function(event) {
                audioChunks.push(event.data);
            };

            mediaRecorder.onstop = function() {
                const elapsedTime = Date.now() - startTime;
                if (elapsedTime < 2000) {
                    console.log(elapsedTime)
                    return;
                }
                const audioBlob = new Blob(audioChunks, { type: 'audio/wav' });
                uploadAudio(audioBlob);
            };
        } catch (error) {
            console.error('Error accessing the microphone', error);
            recording = false;
            setProgress(0);
        }
    }

    function stopRecording() {
        if (!recording || !mediaRecorder) return;
        recording = false;
        setProgress(0);
        mediaRecorder.stop();
    }

    function updateProgress() {
        if (!recording) return;
        const elapsedTime = Date.now() - startTime;
        const progress = (elapsedTime / maxRecordTime) * 100;
        setProgress(progress);

        if (elapsedTime < maxRecordTime) {
            requestAnimationFrame(updateProgress);
        } else {
            stopRecording();
        }
    }

    recordButton.addEventListener('mousedown', startRecording);
    recordButton.addEventListener('mouseup', stopRecording);
    recordButton.addEventListener('mouseleave', stopRecording);
    recordButton.addEventListener('touchstart', startRecording);
    recordButton.addEventListener('touchend', stopRecording);
});

function uploadAudio(blob) {
    fetch('/upload-audio', {
        method: 'POST',
        body: blob
    })
        .then(response => response.json())
        .then(data => console.log(data))
        .catch(error => console.error('Error:', error));
}